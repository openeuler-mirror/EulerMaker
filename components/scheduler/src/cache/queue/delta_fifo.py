from collections import deque
from dataclasses import dataclass
from threading import Lock, Condition
from typing import Any, Callable, Dict, List, Optional, Set

from cache.cache_queue import Queue
from cache.key_funcs import MetaNamespaceKeyFunc
from log import error

# Delta 事件类型常量
DELTA_ADDED = "ADDED"
DELTA_MODIFIED = "MODIFIED"
DELTA_DELETED = "DELETED"
DELTA_SYNC = "SYNC"


@dataclass(frozen=True)
class Delta:
    """单个增量事件。"""
    __slots__ = ('type', 'object')

    type: str    # ADDED | MODIFIED | DELETED | SYNC
    object: Any  # 资源对象（事件发生时的状态快照）


class QueueClosedError(Exception):
    """队列已关闭时抛出的异常。"""
    pass


class DeltaFIFO(Queue):
    """
    增量FIFO队列，用于存储资源类型的增量事件。

    将同一资源的多次变更累积为事件历史列表，消费者一次性获取该资源的完整变更轨迹。

    :param key_func: 键函数，用于将资源对象转换为键
    """

    def __init__(self, key_func: Optional[Callable[[Any], str]] = None):
        # ── 锁 ──────────────────────────────────────────────
        self._lock = Lock()
        self._condition = Condition(self._lock)

        # ── 核心存储 ─────────────────────────────────────────
        # items: key → 该 key 的完整事件历史（按时间升序，index 0 最旧）
        self._items: Dict[str, List[Delta]] = {}

        # queue: FIFO 处理顺序，key 首次出现时追加到右侧
        self._queue: deque[str] = deque()

        # ── 生命周期 ─────────────────────────────────────────
        self._closed = False
        self._synced = False  # 是否已完成首次全量同步（Replace）

        # ── 依赖 ─────────────────────────────────────────────
        self._key_func = MetaNamespaceKeyFunc().key if key_func is None else key_func

    # =========================================================================
    # Producer API（生产者：Reflector）
    # =========================================================================

    def add(self, obj: Any) -> None:
        """添加资源对象（ADDED 事件）。"""
        with self._lock:
            self._queue_locked(DELTA_ADDED, obj)

    def update(self, obj: Any) -> None:
        """更新资源对象（MODIFIED 事件）。"""
        with self._lock:
            self._queue_locked(DELTA_MODIFIED, obj)

    def delete(self, obj: Any) -> None:
        """删除资源对象（DELETED 事件）。"""
        with self._lock:
            self._queue_locked(DELTA_DELETED, obj)

    # =========================================================================
    # Consumer API（消费者：Informer Controller）
    # =========================================================================

    def pop(self) -> List[Delta]:
        """阻塞弹出队首 key 的完整事件历史。

        Returns:
            [Delta, ...] — 队首 key 的事件历史（按时间升序排列），同时从 _items 和 _queue 中删除该 key

        Raises:
            QueueClosedError — 队列已关闭且为空
        """
        with self._lock:
            while self._is_empty_locked() and not self._closed:
                self._condition.wait()
            if self._closed and self._is_empty_locked():
                raise QueueClosedError("queue is closed and empty")
            key = self._queue.popleft()
            deltas = self._items.pop(key)
            return deltas

    def get(self, obj: Any) -> Optional[List[Delta]]:
        """查询指定对象的事件历史（不弹出）。

        返回指定 Delta 的历史列表的浅拷贝。
        Delta 本身不可变, 调用者不能修改返回的列表或 Delta 中的对象。
        即使队列已关闭，仍允许读取尚未消费的事件。
        """
        with self._lock:
            key = self._key_func(obj)
            if key not in self._items:
                return None
            return list[Delta](self._items[key])

    def key_of(self, obj: Any) -> str:
        """提取对象的 key。

        若 obj 是 Delta 实例，则提取其内部对象的 key；否则直接提取 obj 的 key。
        """
        if isinstance(obj, Delta):
            return self._key_func(obj.object)
        return self._key_func(obj)

    def replace(self, obj_list: List[Any], resource_version: str) -> None:
        """全量同步缓存对象（SYNC 事件）。

        删除所有不在 obj_list 中的对象。

        Args:
            obj_list: 新的资源对象列表。
            resource_version: 新列表的资源版本号。

        Raises:
            QueueClosedError — 队列已关闭
        """
        with self._lock:
            if self._closed:
                raise QueueClosedError("queue is closed")

            keys: Set[str] = set[str]()
            for obj in obj_list:
                try:
                    # 一个对象处理失败不影响其他对象
                    keys.add(self._queue_locked(DELTA_SYNC, obj))
                except Exception as e:
                    error(f"error while replacing object {obj}: {e}")
                    continue

            for key, _ in list(self._items.items()):
                if key not in keys:
                    # _items[key] 由 _queue_locked 创建，历史列表必然非空
                    self._queue_locked(DELTA_DELETED, self._items[key][-1].object)

            self._synced = True

            if not self._is_empty_locked():
                self._condition.notify()

    def list(self) -> List[Delta]:
        """返回所有待处理 Delta 的扁平列表。

        遍历 self._items，返回每个 key 的最新 Delta（数组最后一个元素）。
        Delta 本身不可变, 调用者不能修改返回的列表或 Delta 中的对象。
        注意：最新 Delta 可能为 DELETED 类型，调用方需自行判断对象是否仍存活。
        """
        with self._lock:
            result = []
            for _, deltas in self._items.items():
                result.append(deltas[-1])
            return result

    # =========================================================================
    # Lifecycle
    # =========================================================================

    def close(self) -> None:
        """优雅关闭：唤醒所有阻塞在 pop() 的线程。"""
        with self._lock:
            self._closed = True
            self._condition.notify_all()

    def has_synced(self) -> bool:
        """返回是否已完成首次全量同步（Replace() 调用）。

        消费者（Informer Controller）在启动等待阶段轮询此方法，
        确保 Replace() 已完成后再开始处理事件。
        """
        with self._lock:
            return self._synced

    # =========================================================================
    # 内部辅助函数
    # =========================================================================

    def _size_locked(self) -> int:
        """返回队列中待处理 key 的数量。

        调用方必须持有 self._lock。
        """
        return len(self._queue)

    def _is_empty_locked(self) -> bool:
        """返回队列是否为空。

        调用方必须持有 self._lock。
        """
        return not self._queue

    def _queue_locked(self, delta_type: str, obj: Any) -> str:
        """将新 Delta 追加到 key 的事件历史。

        调用方必须持有 self._lock。

        Returns:
            对象对应的 key。
        """
        if self._closed:
            raise QueueClosedError("queue is closed")

        key = self._key_func(obj)

        is_new = key not in self._items
        if is_new:
            self._items[key] = []

        self._items[key].append(Delta(type=delta_type, object=obj))

        self._items[key] = self._dedup_deltas_locked(self._items[key])

        if is_new:
            self._queue.append(key)
            self._condition.notify()

        return key

    def _dedup_deltas_locked(self, deltas: List[Delta]) -> List[Delta]:
        """合并指定 key 的事件历史中连续 DELETED。

        合并规则：仅检查并合并最后两个 Delta（n-1 和 n-2），若均为 DELTA_DELETED 则保留最新的。
        注意：仅在写入时触发，不保证历史中所有连续 DELETED 都被完全去重。

        调用方必须持有 self._lock。
        """
        n = len(deltas)
        if n < 2:
            return deltas

        last = deltas[n - 1]
        second_last = deltas[n - 2]

        if second_last.type == DELTA_DELETED and last.type == DELTA_DELETED:
            deltas[n - 2] = last
            return deltas[:n - 1]

        return deltas
