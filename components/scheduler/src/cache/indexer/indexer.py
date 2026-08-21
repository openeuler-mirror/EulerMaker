from pydantic import Field
from abc import ABC, abstractmethod
from typing import Any, Dict, List, Callable, Mapping, Optional, Set, Tuple, Type

from common import DataContainer, key_func as _key_func
from log import debug


class IndexerError(Exception):
    '''索引器异常基类。'''


class IndexKeyError(IndexerError, KeyError):
    '''索引键相关异常基类, 兼容 except KeyError。'''

    def __str__(self) -> str:
        # KeyError.__str__ 会给消息加引号, 这里还原为纯消息便于日志阅读
        return str(self.args[0]) if self.args else ""


class IndexKeyEmptyError(IndexKeyError):
    '''索引键为空或无效。'''


class IndexKeyConflictError(IndexKeyError):
    '''索引键已存在(新增冲突)。'''


class IndexKeyMissingError(IndexKeyError):
    '''索引键不存在(更新/删除目标缺失)。'''


class IndexHandlerOptions(DataContainer):
    name: str = Field(default=None, description="索引处理器名称")
    description: str = Field(default="", description="索引处理器描述")
    args: Mapping[str, Any] = Field(default_factory=dict, description="索引处理器关键字参数")


class IndexerItemOptions(DataContainer):
    '''索引器选项列表中的单条索引配置。'''

    name: str = Field(default=None, description="索引名称")
    description: str = Field(default="", description="索引器描述")
    handler: IndexHandlerOptions = Field(default=None, description="索引处理器")


class StoreHandlerOptions(DataContainer):
    name: str = Field(default="default", description="数据存储名称")
    description: str = Field(default="", description="数据存储描述")
    args: Mapping[str, Any] = Field(default_factory=dict, description="数据存储关键字参数")


class IndexerOptions(DataContainer):

    # keyFunc 使用驼峰命名以兼容既有 YAML 配置(如 conf/internal.yaml),
    # 与 snake_case 字段并存是有意为之, 新增配置项请使用 snake_case。
    keyFunc: str = Field(default="defaultKeyFunc", description="索引键函数")

    # 索引器选项列表
    indexers: List[IndexerItemOptions] = Field(default_factory=list, description="索引器选项列表")

    # 数据存储选项
    store_handler: StoreHandlerOptions = Field(default_factory=StoreHandlerOptions, description="数据存储选项")

    @property
    def key_func(self) -> Callable[..., Any]:
        '''
        获取索引器的键函数。

        返回:
            索引器的键函数, 用于将数据对象转换为索引键。
        '''
        if self.keyFunc == "defaultKeyFunc":
            return _key_func
        raise ValueError(f"Unrecognized keyFunc: '{self.keyFunc}'. "
                         f"Only 'defaultKeyFunc' is supported.")

    @staticmethod
    def create_from_yaml(yaml_file: str = None) -> "IndexerOptions":
        '''
        从YAML文件创建索引器选项。
        '''
        import yaml
        import os

        # 如果没有指定YAML文件, 则使用默认的internal.yaml
        if not yaml_file:
            yaml_file = os.path.join(os.path.dirname(__file__), "conf/internal.yaml")

            debug(f"load indexer options from yaml file: {yaml_file}")

        try:
            with open(yaml_file, 'r') as f:
                config = yaml.safe_load(f)

                debug(f"read indexers config: {config}")

            return IndexerOptions(**config)
        except FileNotFoundError as e:
            raise FileNotFoundError(f"Indexer config file not found: {yaml_file}") from e
        except yaml.YAMLError as e:
            raise yaml.YAMLError(f"Invalid YAML in indexer config '{yaml_file}': {e}") from e
        except Exception as e:
            # 配置结构错误(字段缺失/类型错误等)统一转为 ValueError,
            # 通过 from e 保留原始异常链, 避免排障时丢失根因
            raise ValueError(f"Failed to load indexer config from '{yaml_file}': {e}") from e


class DataStore:
    def __init__(self) -> None:
        '''
        初始化数据存储。

        资源对象到索引值的映射
        格式: {name: obj}

        支持两阶段提交事务: begin 时浅拷贝正式存储, 后续 save/delete
        等变更先写入事务副本, commit 时引用替换, rollback 时直接丢弃。
        '''
        self._store: Dict[Any, Any] = {}
        self._schema: Dict[Any, Type] = {}

        self._tx_store = None
        self._tx_schema = None

    @property
    def in_transaction(self) -> bool:
        return self._tx_store is not None

    def _active_store(self) -> Dict[Any, Any]:
        return self._tx_store if self.in_transaction else self._store

    def _active_schema(self) -> Dict[Any, Type]:
        return self._tx_schema if self.in_transaction else self._schema

    def save(self, key: Any, data: Any, schema: Type = None) -> None:
        self._active_store()[key] = data
        if schema:
            self._active_schema()[key] = schema
        else:
            self._active_schema().pop(key, None)

    def schema(self, key: Any) -> Optional[Type]:
        return self._active_schema().get(key)

    def get(self, key: Any) -> Any:
        return self._active_store().get(key)

    def exists(self, key: Any) -> bool:
        return key in self._active_store()

    def delete(self, key: Any) -> Tuple[Any, Optional[Type]]:
        store = self._active_store()
        schemas = self._active_schema()
        if key not in store:
            return None, None
        return store.pop(key), schemas.pop(key, None)

    def begin(self) -> None:
        '''开始事务, 浅拷贝正式存储作为事务副本。

        采用浅拷贝而非增量追踪(pending save/delete)的合理性:

        1. 操作粒度匹配: DataStore 的修改以对象为基本单元, save() 替换整个对象,
           delete() 移除整个对象, 不存在"原地修改对象内部字段"的操作。浅拷贝
           共享对象引用不会造成事务内外数据相互污染。

        2. commit 开销更优: 浅拷贝的 commit() 是 O(1) 引用替换, 而增量追踪需要
           O(m) 逐条追写变更(m 为事务内变更量)。

        Raises:
            RuntimeError: 已有事务在进行中。
        '''
        if self.in_transaction:
            raise RuntimeError("DataStore transaction already begun")
        self._tx_store = self._store.copy()
        self._tx_schema = self._schema.copy()

    def commit(self) -> None:
        if not self.in_transaction:
            raise RuntimeError("DataStore transaction not begun")
        self._store = self._tx_store
        self._schema = self._tx_schema
        self._tx_store = None
        self._tx_schema = None

    def rollback(self) -> None:
        if not self.in_transaction:
            raise RuntimeError("DataStore transaction not begun")
        self._tx_store = None
        self._tx_schema = None


class IndexStore:
    def __init__(self) -> None:
        '''
        初始化索引存储。

        索引值到资源对象的映射
        格式: {index_name: {index_value: [name1, name2, ...]}}

        支持两阶段提交事务 + undo_log 回滚:
        - begin: 开启事务, 后续 add/remove 变更记录到 _pending_ops 缓存
        - prepare_commit: 将 _pending_ops 去重优化后写入正式存储, 同时记录
          undo_log 用于回滚逆向
        - finalize_commit: 清空 _pending_ops 和 undo_log
        - rollback: 通过 undo_log 逆向恢复 prepare_commit 的变更, 或直接丢弃
          未写入的 _pending_ops
        '''
        self._indices: Dict[Any, Dict[Any, Set[Any]]] = {}
        # 两阶段提交事务缓存: 非 None 表示 prepare 进行中
        # 格式: [(is_add, index_name, index_value, data_key), ...]
        self._pending_ops = None
        # prepare_commit 写入后的回滚日志: [(is_add, index_name, index_value, data_key), ...]
        # is_add=True 表示原操作是 add, 回滚时需执行 remove; 反之亦然
        self._undo_log = None

    def get_index(self, index_name: Any) -> Mapping[Any, Set[Any]]:
        '''
        获取索引名称对应的数据对象索引。

        返回的是内部索引结构的引用, 仅供查询读取, 调用方禁止修改;
        查询路径已自行复制结果, 不会受到影响。

        Args:
            index_name: 索引名称。
            index_value: 索引值。
        Returns:
            索引名称对应的数据对象索引。
        '''
        return self._indices.get(index_name, {})

    def add_index(self, index_name: Any, index_value: Any, data_key: Any) -> bool:
        '''
        添加数据对象的索引。
        '''
        if self._pending_ops is not None:
            # prepare 阶段: 变更先记录到事务缓存, 不修改正式存储
            self._pending_ops.append((True, index_name, index_value, data_key))
            return True
        return self._apply_add(index_name, index_value, data_key)

    def _apply_add(self, index_name: Any, index_value: Any, data_key: Any) -> bool:
        if index_name not in self._indices:
            self._indices[index_name] = {}

        if index_value not in self._indices[index_name]:
            self._indices[index_name][index_value] = set()

        self._indices[index_name][index_value].add(data_key)

        return True

    def remove_index(self, index_name: Any, index_value: Any, data_key: Any) -> bool:
        '''
        删除数据对象的索引。索引不存在时返回 True（幂等: 目标状态已达到）。
        '''
        if self._pending_ops is not None:
            # prepare 阶段: 变更先记录到事务缓存, 不修改正式存储
            self._pending_ops.append((False, index_name, index_value, data_key))
            return True
        return self._apply_remove(index_name, index_value, data_key)

    def _apply_remove(self, index_name: Any, index_value: Any, data_key: Any) -> bool:
        if index_name not in self._indices:
            return True

        if index_value not in self._indices[index_name]:
            return True

        if data_key not in self._indices[index_name][index_value]:
            return True

        self._indices[index_name][index_value].remove(data_key)

        if len(self._indices[index_name][index_value]) == 0:
            del self._indices[index_name][index_value]

        if not self._indices[index_name]:
            del self._indices[index_name]

        return True

    def begin(self) -> None:
        '''
        开始两阶段提交事务: 后续 add/remove 变更先记录到事务缓存,
        不修改正式存储。

        Raises:
            RuntimeError: 已有事务在进行中。
        '''
        if self._pending_ops is not None:
            raise RuntimeError("IndexStore transaction already begun")
        self._pending_ops = []

    def prepare_commit(self) -> None:
        '''
        将 _pending_ops 去重优化后写入正式存储, 同时记录 undo_log。

        这一步将事务缓存中的索引变更实际应用到 self._indices, 并记录
        每条变更的逆向操作到 undo_log, 以便后续 DataStore 提交失败时
        可通过 rollback 逆向恢复。

        调用方应在 prepare_commit 成功后立即提交 DataStore, 再调用
        finalize_commit 清空缓存。若 DataStore 提交失败, 调用 rollback
        通过 undo_log 恢复 IndexStore 状态。

        Raises:
            RuntimeError: 未处于事务中。
        '''
        if self._pending_ops is None:
            raise RuntimeError("IndexStore transaction not begun")

        self._undo_log = []
        for is_add, index_name, index_value, data_key in self._optimize_ops():
            if is_add:
                self._apply_add(index_name, index_value, data_key)
            else:
                self._apply_remove(index_name, index_value, data_key)
            # 记录逆向操作: is_add=True → 回滚时 remove; is_add=False → 回滚时 add
            self._undo_log.append(
                (is_add, index_name, index_value, data_key)
            )

    def finalize_commit(self) -> None:
        '''
        清空 _pending_ops 和 undo_log, 完成事务提交。

        应在 DataStore.commit() 成功后调用, 表示整个事务已成功提交,
        不再需要回滚。

        Raises:
            RuntimeError: 未处于事务中。
        '''
        if self._pending_ops is None:
            raise RuntimeError("IndexStore transaction not begun")
        self._pending_ops = None
        self._undo_log = None

    def commit(self) -> None:
        '''
        便捷方法: prepare_commit + finalize_commit 一步完成。

        适用于不需要在 IndexStore 和 DataStore 之间插入其他操作的场景。
        若需要 IndexStore.prepare_commit → DataStore.commit → IndexStore.finalize_commit
        的分离流程, 请分别调用三个方法。

        Raises:
            RuntimeError: 未处于事务中。
        '''
        try:
            self.prepare_commit()
        except BaseException:
            self.rollback()
            raise
        self.finalize_commit()

    def rollback(self) -> None:
        '''
        回滚事务: 通过 undo_log 逆向恢复 prepare_commit 的变更, 或直接丢弃
        未写入的 _pending_ops。

        安全幂等: 可安全地在事务任意阶段调用(包括 finalize_commit 之后,
        此时 _pending_ops 和 _undo_log 均为 None, 直接返回)。
        '''
        # 逆向恢复 prepare_commit 已写入的变更
        if self._undo_log is not None:
            for (is_add, index_name, index_value, data_key) in reversed(self._undo_log):
                if is_add:
                    self._apply_remove(index_name, index_value, data_key)
                else:
                    self._apply_add(index_name, index_value, data_key)
            self._undo_log = None
        # 丢弃未写入的 _pending_ops(若 finalize_commit 已调用则已为 None)
        if self._pending_ops is not None:
            self._pending_ops = None

    def _optimize_ops(self) -> List[Tuple[bool, Any, Any, Any]]:
        '''对 _pending_ops 去重优化, 减少 commit 时的冗余操作。

        两层过滤:

        1. Last-write-wins 去重: 以 (index_name, index_value, data_key) 为粒度,
           遍历时后出现的覆盖先出现的, 仅保留最后一条。不同 data_key 的操作
           相互独立, 顺序无关, 因此去重不改变最终语义。

        2. No-op 过滤: 对比初始状态 self._indices, 跳过无实际效果的操作:
           - 最后一条是 add 但 key 已存在于初始索引 → 跳过
           - 最后一条是 remove 但 key 不存在于初始索引 → 跳过

        典型场景:
        - update: remove_index(old_val) + add_index(new_val) → 两条都保留
        - 完全抵消: remove_index(v) + add_index(v) → add 被去重, 若初始
          已存在则为 no-op 跳过, 两条操作全部消除
        - 重复操作: add(v) + add(v) → 仅保留一条, 若初始已存在则跳过'''
        # last-write-wins: 遍历时后出现的覆盖先出现的
        last_ops: Dict[Tuple[Any, Any, Any], bool] = {}
        for is_add, index_name, index_value, data_key in self._pending_ops:
            last_ops[(index_name, index_value, data_key)] = is_add

        result = []
        for (index_name, index_value, data_key), is_add in last_ops.items():
            if is_add:
                # 初始状态已存在则跳过(add 是 no-op)
                if (index_name in self._indices
                        and index_value in self._indices[index_name]
                        and data_key in self._indices[index_name][index_value]):
                    continue
            else:
                # 初始状态不存在则跳过(remove 是 no-op)
                if (index_name not in self._indices
                        or index_value not in self._indices[index_name]
                        or data_key not in self._indices[index_name][index_value]):
                    continue
            result.append((is_add, index_name, index_value, data_key))

        return result


class Indexer(ABC):

    def __init__(self, options: IndexerOptions) -> None:
        self._options = options
        self._data_store = DataStore()
        self._index_store = IndexStore()

    def get_data_store(self) -> DataStore:
        '''
        获取索引器的数据存储。
        '''
        return self._data_store

    def get_index_store(self) -> IndexStore:
        '''
        获取索引器的索引存储。
        '''
        return self._index_store

    @abstractmethod
    def key(self, data: Any) -> Any:
        '''
        提取数据对象的索引键。
        '''
        ...

    @abstractmethod
    def add(self, data: Any) -> Any:
        '''
        添加数据对象到索引中。

        通过两阶段提交保证原子性: prepare 阶段执行数据保存与索引构建,
        任一操作失败或抛出异常则 rollback 并抛出异常, 全部成功才 commit。

        Returns:
            数据对象的唯一标识符。
        '''
        ...

    @abstractmethod
    def update(self, data: Any) -> Any:
        '''
        更新数据对象到索引中。

        通过两阶段提交保证原子性: prepare 阶段执行数据更新与索引重建,
        任一操作失败或抛出异常则 rollback 并抛出异常, 全部成功才 commit。

        Returns:
            数据对象的唯一标识符。
        '''
        ...

    @abstractmethod
    def upsert(self, data: Any) -> Any:
        '''
        原子性地添加或更新数据对象到索引中。

        在同一个锁区间内完成 "存在性判断 + add/update", 避免调用方
        先 exists 再 add/update 两次取锁之间的 TOCTOU 竞态(对象被其他
        线程删除时 update 会抛 KeyError)。

        Returns:
            数据对象的唯一标识符。
        '''
        ...

    @abstractmethod
    def delete(self, data: Any) -> Any:
        '''
        删除数据对象从索引中。

        通过两阶段提交保证原子性: prepare 阶段执行数据删除与索引删除,
        任一操作失败或抛出异常则 rollback 并抛出异常, 全部成功才 commit。

        Returns:
            被删除的数据对象。若数据对象不存在, 则返回None。
        '''
        ...

    @abstractmethod
    def delete_by_key(self, key: Any) -> Any:
        '''
        删除索引器中指定键的数据对象。

        通过两阶段提交保证原子性: prepare 阶段执行数据删除与索引删除,
        任一操作失败或抛出异常则 rollback 并抛出异常, 全部成功才 commit。

        Returns:
            被删除的数据对象。若数据对象不存在, 则返回None。
        '''
        ...

    @abstractmethod
    def query(self, index_name: str, index_value: Any) -> List[Any]:
        '''
        查询索引值对应的数据对象列表。
        '''
        ...

    @abstractmethod
    def query_one(self, index_name: str, index_value: Any) -> Any:
        '''
        查询索引值对应的数据对象。
        '''
        ...

    @abstractmethod
    def exists(self, data: Any) -> bool:
        '''
        判断数据对象是否存在。
        '''
        ...


class KeyValue(DataContainer):
    key: Any = Field(default=None, description="索引键")
    value: Any = Field(default=None, description="索引值")

    def __eq__(self, other: object) -> bool:
        if not isinstance(other, KeyValue):
            return NotImplemented
        return self.key == other.key and self.value == other.value

    def __hash__(self) -> int:
        def _to_hashable(v):
            """将值转为可哈希类型; 对 list/dict/set 等不可哈希类型降级为 repr(v)。"""
            try:
                hash(v)
                return v
            except TypeError:
                return repr(v)
        return hash((_to_hashable(self.key), _to_hashable(self.value)))
