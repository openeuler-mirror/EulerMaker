from abc import ABC, abstractmethod
from typing import Any, Callable, List, Mapping, Optional, Tuple, Type

from pydantic import BaseModel
from .indexer import (
    Indexer,
    IndexKeyEmptyError,
    IndexKeyConflictError,
    IndexKeyMissingError,
)


class StoreHandler(ABC):
    def __init__(self, indexer: Indexer, **kwargs):
        self._indexer = indexer

    @abstractmethod
    def save_data(self, data: Any) -> Any:
        '''
        保存数据对象到数据存储中。

        失败时抛出异常(如 key 无效时抛出 IndexKeyEmptyError、
        已存在时抛出 IndexKeyConflictError),
        不允许静默返回失败; 由 Indexer 两阶段提交负责回滚。
        '''
        ...

    @abstractmethod
    def delete_data(self, data: Any) -> Any:
        '''
        删除数据对象从数据存储中。

        数据对象不存在时返回 None(幂等); 其余失败抛出异常,
        不允许静默返回失败。
        '''
        ...

    @abstractmethod
    def update_data(self, data: Any) -> Any:
        '''
        更新数据对象到数据存储中。

        数据对象不存在或 key 无效时抛出 IndexKeyMissingError, 其余失败同样抛出异常,
        不允许静默返回失败; 由 Indexer 两阶段提交负责回滚。
        '''
        ...

    @abstractmethod
    def query_data(self, keys: List[Any]) -> List[Any]:
        '''
        查询所有数据对象从数据存储中。
        '''
        ...

    @abstractmethod
    def query_by_key(self, key: Any) -> Any:
        '''
        查询数据对象从数据存储中。
        '''
        ...

    @abstractmethod
    def exists(self, data: Any) -> bool:
        '''
        检查数据对象是否存在于数据存储中。
        '''
        ...

    @abstractmethod
    def delete_by_key(self, key: Any) -> Any:
        '''
        从数据存储中删除指定键的数据对象并返回被删除的数据。
        若数据对象不存在, 则返回 None。
        '''
        ...


__STORE_HANDLERS__: Mapping[str, Type[StoreHandler]] = {}


def store_handler(name: str) -> Callable[[Type[StoreHandler]], Type[StoreHandler]]:
    '''
    注册数据存储处理器。

    参数:
        name (str): 数据存储处理器名称。

    返回:
        Type[StoreHandler]: 数据存储处理器类。
    '''
    def wrapper(cls: Type[StoreHandler]) -> Type[StoreHandler]:
        __STORE_HANDLERS__[name] = cls
        return cls
    return wrapper


def create_store_handler(name: str, indexer: Indexer, **kwargs) -> StoreHandler:
    '''
    创建数据存储处理器。

    参数:
        name (str, optional): 数据存储处理器名称。
        indexer (Indexer): 索引器实例。

    返回:
        StoreHandler: 数据存储处理器实例。
    '''
    if name not in __STORE_HANDLERS__:
        raise ValueError(f"store handler {name} not registered")

    return __STORE_HANDLERS__[name](indexer, **kwargs)


@store_handler("default")
class DefaultStoreHandler(StoreHandler):
    def __init__(self, indexer: Indexer, **kwargs) -> None:
        super().__init__(indexer, **kwargs)

    def _validate_key(self, data: Any) -> Any:
        '''
        校验并提取数据对象的 key。key 无效时抛出 IndexKeyEmptyError。
        '''
        _key = self._indexer.key(data)
        if _key is None or _key == "" or (isinstance(_key, (list, tuple, dict)) and len(_key) == 0):
            raise IndexKeyEmptyError("index key is empty")
        return _key

    def _serialize(self, data: Any) -> Tuple[Any, Optional[Type]]:
        '''
        序列化数据对象: BaseModel 转为 JSON bytes 并附带其 schema, 其余原样返回(schema 为 None)。
        以紧凑字节流驻留内存是有意为之(dict 对象图占用约为 JSON bytes 的 6 倍);
        使用 pydantic 内置序列化(Rust 实现), 比 json.dumps(model_dump()) 更快且无中间 dict。
        '''
        schema = type(data) if isinstance(data, BaseModel) else None
        if schema is not None:
            data = data.model_dump_json().encode("utf-8")
        return data, schema

    def _deserialize(self, raw: Any, schema: Optional[Type]) -> Any:
        '''
        反序列化数据对象: 有 schema 时从 JSON bytes 重建, 否则原样返回。
        model_validate_json 直接从 bytes 校验构建, 不产生中间 dict, 且能触发
        mode='before' 模型校验器(便于后续存储格式演进)。
        '''
        return schema.model_validate_json(raw) if schema is not None else raw

    def save_data(self, data: Any) -> Any:
        '''
        保存数据对象到数据存储中。

        失败时抛出异常: key 无效时抛出 IndexKeyEmptyError,
        已存在时抛出 IndexKeyConflictError。

        Returns:
            数据对象的唯一标识符。
        '''
        _key = self._validate_key(data)

        store = self._indexer.get_data_store()

        if store.exists(_key):
            raise IndexKeyConflictError(f"index key {_key} already exists")

        serialized, schema = self._serialize(data)
        store.save(_key, serialized, schema)

        return _key

    def delete_data(self, data: Any) -> Any:
        '''
        删除数据对象从数据存储中。

        数据对象不存在时返回 None(幂等)。

        Returns:
            被删除的数据对象; 数据对象不存在时返回 None。
        '''
        _key = self._validate_key(data)

        store = self._indexer.get_data_store()

        raw, schema = store.delete(_key)
        if raw is None:
            return None

        return self._deserialize(raw, schema)

    def update_data(self, data: Any) -> Any:
        '''
        更新数据对象到数据存储中。

        数据对象不存在时抛出 IndexKeyMissingError(调用方应先通过 exists 判断,
        或按新增处理), 避免"旧数据缺失却静默写入新数据、索引未重建"
        造成的数据与索引不一致。

        Args:
            data: 数据对象。

        Returns:
            更新前的旧数据对象。

        Raises:
            IndexKeyMissingError: key 无效或数据对象不存在。
        '''
        _key = self._validate_key(data)

        store = self._indexer.get_data_store()

        if not store.exists(_key):
            raise IndexKeyMissingError(f"index key {_key} not exists")

        # 用写入时存储的 schema 重建旧数据(而不是新数据的类型), 避免新旧类型不一致时误解析
        old_data = self._deserialize(store.get(_key), store.schema(_key))

        new_data, new_schema = self._serialize(data)
        store.save(_key, new_data, new_schema)

        return old_data

    def query_data(self, keys: List[Any]) -> List[Any]:
        '''
        查询所有数据对象从数据存储中。

        索引指向但数据已不存在的 key 直接跳过, 不在结果中混入 None。
        '''
        store = self._indexer.get_data_store()

        results = []
        for k in keys:
            raw = store.get(k)
            if raw is None:
                continue
            results.append(self._deserialize(raw, store.schema(k)))
        return results

    def query_by_key(self, key: Any) -> Any:
        '''
        查询数据对象从数据存储中。

        数据不存在时返回 None。
        '''
        store = self._indexer.get_data_store()

        raw = store.get(key)
        if raw is None:
            return None
        return self._deserialize(raw, store.schema(key))

    def delete_by_key(self, key: Any) -> Any:
        '''
        从数据存储中删除指定键的数据对象并返回被删除的数据。
        若数据对象不存在, 则返回 None。
        '''
        store = self._indexer.get_data_store()

        raw, schema = store.delete(key)
        if raw is None:
            return None

        return self._deserialize(raw, schema)

    def exists(self, data: Any) -> bool:
        '''
        检查数据对象是否存在于数据存储中。
        key 无效时抛出 IndexKeyEmptyError(与 save_data/delete_data/update_data 契约一致)。
        '''
        _key = self._validate_key(data)
        return self._indexer.get_data_store().exists(_key)
