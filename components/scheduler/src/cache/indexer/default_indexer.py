from contextlib import contextmanager
from threading import RLock
from typing import Any, Generator, List, Mapping

from log import info, debug

from .indexer import Indexer, IndexerOptions
from .index_handlers import IndexHandler, create_index_handler
from .store_handlers import StoreHandler, create_store_handler


class DefaultIndexer(Indexer):
    def __init__(self, options: IndexerOptions = None) -> None:
        if options is None:
            options = IndexerOptions.create_from_yaml()
        # 初始化索引器
        super().__init__(options)

        # 索引值类型到处理函数的映射
        self._handlers: Mapping[Any, IndexHandler] = {}

        # 数据存储处理器
        self._store_handler: StoreHandler = None

        # 索引器锁
        self._lock = RLock()

        self._initialize_handlers()

    def _initialize_handlers(self):
        '''
        初始化索引处理器。
        '''
        if not self._options.indexers or len(self._options.indexers) == 0:
            raise ValueError("indexers is empty")

        for idx in self._options.indexers:
            # 配置校验: 尽早暴露缺失/重复的索引定义, 避免运行时才报出误导性错误
            if not idx.name:
                raise ValueError("index name is not specified")
            if idx.handler is None:
                raise ValueError(f"index {idx.name}: handler is not specified")
            if not idx.handler.name:
                raise ValueError(f"index {idx.name}: handler name is not specified")
            if idx.name in self._handlers:
                raise ValueError(f"index {idx.name} is duplicated")

            args = idx.handler.args or {}

            if 'indexName' not in args:
                args['indexName'] = idx.name

            h = create_index_handler(self, idx.handler.name, **args)
            self._handlers[idx.name] = h
            info(f"index {idx.name} initialized with handler {idx.handler.name} registered")

        # 初始化数据存储处理器
        store_handler_name = self._options.store_handler.name
        store_handler_args = self._options.store_handler.args or {}
        self._store_handler = create_store_handler(store_handler_name, self, **store_handler_args)
        info(f"store handler {store_handler_name} initialized")

    def _get_handler(self, name: str) -> IndexHandler:
        '''获取并校验索引处理器, 未注册时抛出 ValueError。'''
        h = self._handlers.get(name, None)
        if not h:
            raise ValueError(f"index handler {name} is not registered")
        return h

    def _delete_indices(self, data: Any, log_label: str) -> None:
        '''遍历所有索引处理器删除数据的索引条目。

        delete_index 返回 False 仅表示可选字段无值、跳过删除(非失败);
        真正的失败以异常抛出, 由两阶段提交统一回滚。
        '''
        for idx in self._options.indexers:
            h = self._get_handler(idx.name)
            if not h.delete_index(data):
                debug(f"index {idx.name} delete skipped for {log_label}")

    @contextmanager
    def _transaction(self) -> Generator[None, None, None]:
        '''
        两阶段提交事务上下文(IndexStore 含 undo_log 回滚):

            prepare        - 数据/索引变更写入事务缓存, 正式存储保持不变;
            prepare_commit - IndexStore 将变更写入正式存储, 记录 undo_log;
            commit         - DataStore 提交, 正式存储引用替换;
            finalize       - IndexStore 清空事务缓存和 undo_log;
            rollback       - 任一阶段失败或异常, 通过 undo_log 逆向恢复
                             IndexStore 变更, DataStore 丢弃副本, 重新抛出异常。
        '''
        with self._lock:
            data_started = False
            index_started = False

            try:
                self._data_store.begin()
                data_started = True

                self._index_store.begin()
                index_started = True

                yield
                self._index_store.prepare_commit()
            except BaseException:
                if index_started:
                    try:
                        self._index_store.rollback()
                    except BaseException:
                        pass
                if data_started:
                    try:
                        self._data_store.rollback()
                    except BaseException:
                        pass
                raise

            # commit 失败 → 仍需 rollback IndexStore
            try:
                self._data_store.commit()
            except BaseException:
                self._index_store.rollback()
                raise

            self._index_store.finalize_commit()

    def key(self, data: Any) -> Any:
        '''
        提取数据对象的索引键。
        '''
        return self._options.key_func(data)

    def add(self, data: Any) -> Any:
        '''
        添加数据对象到索引中。

        两阶段提交保证原子性: prepare 执行 StoreHandler.save_data 与各
        IndexHandler.build_index, 全部成功才 commit, 任一失败或异常
        则 rollback 并抛出异常。

        Args:
            data: 数据对象。
        Returns:
            数据对象的唯一标识符。

        Raises:
            IndexKeyEmptyError: 索引键无效。
            IndexKeyConflictError: 索引键已存在。
            ValueError: required 索引值缺失或索引值类型不受支持。
        '''
        with self._transaction():
            # 数据操作: 存储数据
            _key = self._store_handler.save_data(data)

            # 索引操作: 构建索引
            for idx in self._options.indexers:
                self._get_handler(idx.name).build_index(data)
        return _key

    def update(self, data: Any) -> Any:
        '''
        更新数据对象到索引中。

        两阶段提交保证原子性: prepare 执行 StoreHandler.update_data 与各
        IndexHandler.rebuild_index, 全部成功才 commit, 任一失败或异常
        则 rollback 并抛出异常。

        Args:
            data: 数据对象。
        Returns:
            数据对象的唯一标识符。

        Raises:
            IndexKeyMissingError: 索引键无效或数据对象不存在。
            ValueError: required 索引值缺失或索引值类型不受支持。
        '''
        with self._transaction():
            # 数据操作: 更新数据
            old_data = self._store_handler.update_data(data)

            # 索引操作: 重建索引
            for idx in self._options.indexers:
                self._get_handler(idx.name).rebuild_index(data, old_data)
        return self.key(data)

    def upsert(self, data: Any) -> Any:
        '''
        原子性地添加或更新数据对象到索引中。

        在同一个锁区间内完成 "存在性判断 + add/update", 消除调用方
        "先 exists 再 add/update" 两次取锁之间的 TOCTOU 竞态:
        判断存在后其他线程无法插入删除, update 不会再因对象缺失抛 KeyError。

        Args:
            data: 数据对象。
        Returns:
            数据对象的唯一标识符。
        '''
        with self._lock:
            if self.exists(data):
                return self.update(data)
            return self.add(data)

    def delete(self, data: Any) -> Any:
        '''
        删除数据对象从索引中。

        两阶段提交保证原子性: prepare 执行 StoreHandler.delete_data 与各
        IndexHandler.delete_index, 全部成功才 commit, 任一失败或异常
        则 rollback 并抛出异常。

        Args:
            data: 数据对象。
        Returns:
            被删除的数据对象。若数据对象不存在, 则返回None。
        '''
        with self._transaction():
            # 数据操作: 删除数据
            rm_data = self._store_handler.delete_data(data)

            # 索引操作: 删除索引
            if rm_data is not None:
                self._delete_indices(rm_data, f"data {rm_data}")
        return rm_data

    def delete_by_key(self, key: Any) -> Any:
        '''
        删除索引器中指定键的数据对象。

        两阶段提交保证原子性: prepare 执行 StoreHandler.delete_by_key 与各
        IndexHandler.delete_index, 全部成功才 commit, 任一失败或异常
        则 rollback 并抛出异常。

        Args:
            key: 数据对象的唯一标识符。
        Returns:
            被删除的数据对象。若数据对象不存在, 则返回None。
        '''
        with self._transaction():
            # 数据操作: 删除数据
            _data = self._store_handler.delete_by_key(key)

            # 索引操作: 删除索引
            if _data is not None:
                self._delete_indices(_data, f"key {key}")
        return _data

    def query(self, index_name: str, index_value: Any) -> List[Any]:
        '''
        查询索引值对应的数据对象列表。

        Args:
            index_name: 索引名称。
            index_value: 索引值。
        Returns:
            索引值对应的数据对象列表。若索引值不存在, 则返回空列表。
        '''
        with self._lock:
            if index_name not in self._handlers:
                return []

            data_keys = self._handlers[index_name].query(index_value)

            if not data_keys:
                return []

            return self._store_handler.query_data(data_keys)

    def query_one(self, index_name: str, index_value: Any) -> Any:
        '''
        查询索引值对应的数据对象。
        '''
        with self._lock:
            if index_value is None or index_name is None:
                return None

            data_list = self.query(index_name, index_value)

            if len(data_list) == 0:
                return None

            return data_list[0]

    def exists(self, data: Any) -> bool:
        '''
        判断数据对象是否存在。
        '''
        with self._lock:
            return self._store_handler.exists(data)
