from abc import ABC, abstractmethod
from typing import Any, Callable, Mapping, List, Optional, Type


from .indexer import Indexer, KeyValue
from common import find_index_value


__INDEX_HANDLERS__: Mapping[str, Type["IndexHandler"]] = {}

# _required_fallback 的哨兵返回值: 表示"调用方应直接 return None"
_FALLBACK_UNSET = object()


def index_handler(name: str) -> Callable[[Type["IndexHandler"]], Type["IndexHandler"]]:
    '''
    获取索引处理器注册装饰器。
    '''
    def decorator(cls: Type["IndexHandler"]) -> Type["IndexHandler"]:
        __INDEX_HANDLERS__[name] = cls
        return cls
    return decorator


def create_index_handler(indexer: Indexer, handler_name: str, *args, **kwargs) -> "IndexHandler":
    '''
    创建索引处理器实例。
    '''
    if handler_name not in __INDEX_HANDLERS__:
        raise ValueError(f"Index handler {handler_name} not registered.")

    handler_cls = __INDEX_HANDLERS__[handler_name]
    return handler_cls(indexer, *args, **kwargs)


class IndexHandler(ABC):
    '''
    IndexHandler类, 用于处理索引器的索引值。

    方法契约:
        - 返回 False 仅表示"跳过"(可选字段无值, 无需构建/删除索引), 不是失败;
        - 真正的失败(如 required 字段缺失且无默认值、索引值类型不受支持等)
          必须抛出异常, 不允许静默返回 False;
        - 调用方(Indexer)通过两阶段提交统一处理异常: prepare 任一操作
          失败或异常则 rollback, 全部成功才 commit。
    '''

    def __init__(self, indexer: Indexer, *args, **kwargs):
        # 注意: 子类须在调用 super().__init__ 之前设置 _supported_types,
        # 以便这里校验 defaultValue 的类型。
        if not hasattr(self, '_supported_types'):
            raise TypeError(
                f"{type(self).__name__} must set _supported_types before calling super().__init__()"
            )
        self._indexer = indexer

        if 'indexName' not in kwargs:
            raise ValueError("indexName must be specified.")
        self._index_name = kwargs['indexName']

        self._max_depth = 7
        if 'maxDepth' in kwargs:
            self._max_depth = kwargs['maxDepth']

        if 'retrieval' not in kwargs:
            raise ValueError("retrieval must be specified.")
        self._retrieval = kwargs['retrieval']

        self._required = False
        if 'required' in kwargs:
            self._required = kwargs['required']

        self._default_value = None
        if 'defaultValue' in kwargs:
            self._default_value = kwargs['defaultValue']
            if self._default_value is not None and type(self._default_value) not in self._supported_types:
                raise ValueError(
                    f"defaultValue for {type(self).__name__} must be of type {self._supported_types}, "
                    f"got {type(self._default_value)}"
                )

    def _required_fallback(self, default_usable: bool, soft: bool) -> Any:
        '''
        required 校验失败时的兜底处理(值为 None 或为空两处共用):
        - 非 required: 返回 _FALLBACK_UNSET, 调用方应 return None;
        - required 且默认值可用: 返回默认值;
        - required 且默认值不可用: soft 模式返回 _FALLBACK_UNSET(降级跳过), 否则抛出 ValueError。
        '''
        if not self._required:
            return _FALLBACK_UNSET
        if not default_usable:
            if soft:
                return _FALLBACK_UNSET
            raise ValueError(f"Index {self._retrieval} is required but no valid value was found.")
        return self._default_value

    def _validate_index_value(self, data: Any, check_empty: bool = False, soft: bool = False) -> Optional[Any]:
        '''
        校验并获取索引值。返回 None 表示调用方应 return False，返回有效值表示校验通过。
        soft 模式下非法值降级为 None 而非抛出异常(用于 rebuild 时移除旧条目,
        见 _old_index_value)。
        '''
        index_value = find_index_value(data, self._retrieval, self._max_depth, self._default_value)
        if index_value is None:
            fallback = self._required_fallback(self._default_value is not None, soft)
            if fallback is _FALLBACK_UNSET:
                return None
            index_value = fallback

        if type(index_value) not in self._supported_types:
            if soft:
                return None
            raise ValueError(f"Index value of type {type(index_value)} is not supported.")

        # 仅对可求长度的值判空: 数值型等无 __len__ 的值不参与 check_empty,
        # 避免未来新增数值型 handler 传 check_empty=True 时 len() 抛 TypeError
        if check_empty and hasattr(index_value, '__len__') and len(index_value) == 0:
            fallback = self._required_fallback(
                self._default_value is not None and len(self._default_value) != 0, soft)
            if fallback is _FALLBACK_UNSET:
                return None
            index_value = fallback

        return index_value

    def _old_index_value(self, data: Any, check_empty: bool = False) -> Optional[Any]:
        '''
        计算旧数据建索引时所用的索引值(用于 rebuild 时移除旧条目)。
        与 _validate_index_value 共享同一套校验与 defaultValue 兜底规则,
        但旧值非法时降级为 None(跳过移除)而非抛出异常: 此类数据在 build_index
        时本就会失败、不存在旧条目, rebuild 不应因此被中断。
        '''
        return self._validate_index_value(data, check_empty=check_empty, soft=True)

    @abstractmethod
    def build_index(self, data: Any) -> bool:
        '''
        构建数据索引。

        Returns:
            True 表示索引已构建; False 表示跳过(可选字段无值, 非失败)。

        Raises:
            ValueError: 索引值缺失且 required 无默认值, 或索引值类型不受支持。
        '''
        ...

    @abstractmethod
    def rebuild_index(self, new_data: Any, old_data: Any) -> bool:
        '''
        重建数据索引。

        Returns:
            True 表示索引已重建; False 表示跳过(可选字段无值, 非失败)。

        Raises:
            ValueError: 新索引值缺失且 required 无默认值, 或索引值类型不受支持。
        '''
        ...

    @abstractmethod
    def delete_index(self, data: Any) -> bool:
        '''
        删除数据索引。

        Returns:
            True 表示索引已删除; False 表示跳过(可选字段无值, 非失败)。

        Raises:
            ValueError: 索引值缺失且 required 无默认值, 或索引值类型不受支持。
        '''
        ...

    @abstractmethod
    def query(self, index_value: Any) -> List[Any]:
        '''
        查询索引值对应的资源对象列表。
        '''
        ...


@index_handler("GeneralHandler")
class GeneralHandler(IndexHandler):
    '''
    通用索引处理器, 支持int、float、str、bool类型的索引值。
    '''

    def __init__(self, indexer: Indexer, **kwargs: Any) -> None:
        '''
        初始化通用索引处理器。
        '''
        self._supported_types = [int, float, str, bool]
        super().__init__(indexer, **kwargs)

    def build_index(self, data: Any) -> bool:
        '''
        构建索引。
        '''
        index_value = self._validate_index_value(data)
        if index_value is None:
            return False

        _key = self._indexer.key(data)

        # 添加索引
        return self._indexer.get_index_store().add_index(self._index_name, index_value, _key)

    def rebuild_index(self, new_data: Any, old_data: Any) -> bool:
        # 先校验新索引值
        index_value = self._validate_index_value(new_data)

        # 删除旧索引
        if old_data is not None:
            old_index_value = self._old_index_value(old_data)
            if old_index_value is not None:
                _old_key = self._indexer.key(old_data)
                self._indexer.get_index_store().remove_index(self._index_name, old_index_value, _old_key)

        if index_value is None:
            return False

        # 添加索引
        _key = self._indexer.key(new_data)
        return self._indexer.get_index_store().add_index(self._index_name, index_value, _key)

    def delete_index(self, data: Any) -> bool:
        '''
        删除索引。
        '''
        index_value = self._validate_index_value(data)
        if index_value is None:
            return False

        _key = self._indexer.key(data)

        # 删除索引
        return self._indexer.get_index_store().remove_index(self._index_name, index_value, _key)

    def query(self, index_value: Any) -> List[Any]:
        '''
        查询索引值对应的资源对象列表。
        '''
        if index_value is None:
            return []

        if type(index_value) not in self._supported_types:
            raise ValueError(f"Index value of type {type(index_value)} is not supported.")

        # 查询索引
        index_dict = self._indexer.get_index_store().get_index(self._index_name)

        if index_value in index_dict:
            return [x for x in index_dict[index_value]]
        return []


@index_handler("ListHandler")
class ListHandler(IndexHandler):
    '''
    列表索引处理器, 支持list类型的索引值。
    '''

    def __init__(self, indexer: Indexer, **kwargs: Any) -> None:
        '''
        初始化列表索引处理器。
        '''
        self._supported_types = [list]
        self._query_supported_types = [list, str, int, float, bool]
        super().__init__(indexer, **kwargs)

    def build_index(self, data: Any) -> bool:
        '''
        构建索引。
        '''
        index_value = self._validate_index_value(data, check_empty=True)
        if index_value is None:
            return False

        _key = self._indexer.key(data)

        for item in index_value:
            if not self._indexer.get_index_store().add_index(self._index_name, item, _key):
                return False

        return True

    def rebuild_index(self, new_data: Any, old_data: Any) -> bool:
        # 先校验新索引值
        index_value = self._validate_index_value(new_data, check_empty=True)

        # 删除旧索引
        if old_data is not None:
            old_index_value = self._old_index_value(old_data, check_empty=True)
            if old_index_value is not None:
                _old_key = self._indexer.key(old_data)
                for item in old_index_value:
                    self._indexer.get_index_store().remove_index(self._index_name, item, _old_key)

        if index_value is None:
            return False

        # 添加新索引
        _key = self._indexer.key(new_data)
        for item in index_value:
            if not self._indexer.get_index_store().add_index(self._index_name, item, _key):
                return False

        return True

    def delete_index(self, data: Any) -> bool:
        '''
        删除索引。
        '''
        index_value = self._validate_index_value(data, check_empty=True)
        if index_value is None:
            return False

        _key = self._indexer.key(data)

        for item in index_value:
            if not self._indexer.get_index_store().remove_index(self._index_name, item, _key):
                return False

        return True

    def query(self, index_value: Any) -> List[Any]:
        '''
        查询索引值对应的资源对象列表。
        '''
        if index_value is None:
            return []

        if type(index_value) not in self._query_supported_types:
            raise ValueError(f"Index value of type {type(index_value)} is not supported.")

        if type(index_value) != list:
            index_value = [index_value]

        if len(index_value) == 0:
            return []

        data_key = set()
        index_dict = self._indexer.get_index_store().get_index(self._index_name)

        for item in index_value:
            if item in index_dict:
                # 查询索引
                data_key.update(index_dict[item])
        return list(data_key)


@index_handler("DictHandler")
class DictHandler(IndexHandler):
    '''
    字典索引处理器, 支持dict类型的索引值。
    '''

    def __init__(self, indexer: Indexer, **kwargs: Any) -> None:
        '''
        初始化字典索引处理器。
        '''
        self._supported_types = [dict]
        super().__init__(indexer, **kwargs)

    def build_index(self, data: Any) -> bool:
        '''
        构建索引。
        '''
        index_value = self._validate_index_value(data, check_empty=True)
        if index_value is None:
            return False

        _key = self._indexer.key(data)

        for k, v in index_value.items():
            if not self._indexer.get_index_store().add_index(self._index_name, KeyValue(key=k, value=v), _key):
                return False

        return True

    def rebuild_index(self, new_data: Any, old_data: Any) -> bool:
        # 先校验新索引值
        index_value = self._validate_index_value(new_data, check_empty=True)

        # 删除旧索引
        if old_data is not None:
            old_index_value = self._old_index_value(old_data, check_empty=True)
            if old_index_value is not None:
                _old_key = self._indexer.key(old_data)
                for k, v in old_index_value.items():
                    self._indexer.get_index_store().remove_index(self._index_name, KeyValue(key=k, value=v), _old_key)

        if index_value is None:
            return False

        # 添加新索引
        _key = self._indexer.key(new_data)
        for k, v in index_value.items():
            if not self._indexer.get_index_store().add_index(self._index_name, KeyValue(key=k, value=v), _key):
                return False

        return True

    def delete_index(self, data: Any) -> bool:
        '''
        删除索引。
        '''
        index_value = self._validate_index_value(data, check_empty=True)
        if index_value is None:
            return False

        _key = self._indexer.key(data)

        for k, v in index_value.items():
            if not self._indexer.get_index_store().remove_index(self._index_name, KeyValue(key=k, value=v), _key):
                return False

        return True

    def query(self, index_value: Any) -> List[Any]:
        '''
        查询索引值对应的资源对象列表。
        '''
        if index_value is None:
            return []

        if type(index_value) not in self._supported_types:
            raise ValueError(f"Index value of type {type(index_value)} is not supported.")

        if len(index_value) == 0:
            return []

        data_key = None
        index_dict = self._indexer.get_index_store().get_index(self._index_name)

        for k, v in index_value.items():
            kv = KeyValue(key=k, value=v)

            # AND 操作: 所有键值对都存在索引时才返回结果
            if kv in index_dict:
                existing_keys = set(index_dict[kv])
                # 空索引值: 无匹配资源
                if not existing_keys:
                    return []
                # 初始化或合并索引值对应的资源键, 交集操作确保结果只包含同时存在于所有索引值中的资源键
                data_key = existing_keys if data_key is None else data_key & existing_keys
            else:
                return []
        return list(data_key) if data_key else []
