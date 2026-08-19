"""
工作队列实现。

基于 Heap 实现双队列调度模型，支持去重、优先级排序、有主状态、优雅停止和指数退避。

参考 Kubernetes PriorityQueue 的设计：
- items map 实现 O(1) 去重
- heap.Interface + Fix() 实现 O(log n) 动态优先级更新
- inFlight + dirty 标记实现"有主"状态

两阶段设计：
- Phase 1: PriorityQueue — 基础优先级队列
- Phase 2: WorkQueue(PriorityQueue) — 增加指数退避能力
"""

import math
import threading
import time
from typing import Dict, List, Optional, Set

from cache.heap import Heap


# =============================================================================
# Item 调度项模型
# =============================================================================

class Item:
    """队列调度项，与业务对象解耦。

    使用 __slots__ 减少内存开销。
    Phase 1 只使用 key、priority、_sequence 三个字段。
    """

    __slots__ = ('key', 'priority', '_sequence')

    def __init__(self, key: str, priority: int = 0) -> None:
        self.key = key                    # 唯一标识，用于去重
        self.priority = priority          # 调度优先级，值越大越优先
        self._sequence: int = 0           # 自增序列，用于同优先级 FIFO 排序


class BackoffItem(Item):
    """扩展 Item，增加退避相关字段。

    Phase 2 使用，WorkQueue.add_backoff() 需要 BackoffItem 实例。
    """

    __slots__ = ('_backoff_time', '_retry_count')

    def __init__(self, key: str, priority: int = 0) -> None:
        super().__init__(key, priority)
        self._backoff_time: float = 0.0      # 退避到期时间（monotonic 时钟）
        self._retry_count: int = 0           # 退避重试次数


# =============================================================================
# PriorityQueue
# =============================================================================

class PriorityQueue:
    """线程安全的优先级队列，支持去重、有主状态、优雅停止。

    职责：
    - 去重：基于 Heap 的 key 映射，同一 key 的 Item 自动覆盖
    - 优先级排序：基于 Heap 的 less_func，高优先级先出队，同优先级 FIFO
    - 有主状态：get() 返回的 Item 标记为 in_flight，done() 前不会被重复取出
    - 优雅停止：close() 唤醒所有等待线程，get() 返回 None；
      关闭后剩余 Item 不会被继续取出，可用 size()/pending() 检查或用 delete() 清理
    - 线程安全：threading.Condition 保护所有共享状态
    - 容量：max_size 只限制 active_q 中的全新任务；处理期间新到的更新
      （dirty）重入队时豁免容量检查，避免更新静默丢失

    使用示例：
        q = PriorityQueue()
        q.add(Item("job-1", priority=10))
        q.add(Item("job-2", priority=5))

        item = q.get()       # 返回 job-1
        q.done(item)         # 标记处理完成
    """

    def __init__(self, max_size: int = 0) -> None:
        if max_size < 0:
            raise ValueError(
                f"max_size must be greater than or equal to 0, but got {max_size}"
            )

        # ── 锁 ────────────────────────────────────────────────────
        self._lock = threading.Lock()
        self._cond = threading.Condition(self._lock)

        # ── 活跃队列 ──────────────────────────────────────────────
        # 大顶堆：高优先级在前；同优先级时，先入队的在前
        self._active_q = Heap(
            key_func=lambda item: item.key,
            less_func=self._priority_less,
        )

        # ── 有主状态 ──────────────────────────────────────────────
        self._in_flight: Dict[str, Item] = {}   # 正在处理中的 Item
        self._dirty: Set[str] = set()           # 处理期间被重新 add 的 key

        # ── 生命周期 ──────────────────────────────────────────────
        self._closed: bool = False

        # ── 自增序列 ──────────────────────────────────────────────
        self._sequence: int = 0         # 自增序列，保证同优先级 FIFO；Python int 无溢出，无需处理回绕

        # ── 容量 ──────────────────────────────────────────────────
        self._max_size: int = max_size          # 0 表示无限制

    # ════════════════════════════════════════════════════════════════
    # 公共接口（自动获取锁）
    # ════════════════════════════════════════════════════════════════

    def add(self, item: Item) -> bool:
        """添加或更新 Item。

        语义：
        - 已在 active_q 中：更新优先级，保留 _sequence
        - 已在 in_flight 中：标记 dirty，done() 后重新入队
        - 全新 Item：分配自增序列号，入队
        - 队列满：返回 False（重试策略由调用方决定）
        - 已关闭：返回 False

        Returns:
            True 表示成功入队/更新；False 表示队列已关闭，
            或队列满且 Item 是全新的。
        """
        with self._cond:
            return self._add_locked(item)

    def get(self) -> Optional[Item]:
        """阻塞获取下一个待处理的 Item。

        返回的 Item 具有"有主"状态：在 done() 被调用前，
        该 Item 不会被重复取出，add() 会将其标记为 dirty。

        Returns:
            Item 对象；队列已关闭时返回 None。
        """
        with self._cond:
            return self._get_locked()

    def done(self, item: Item) -> None:
        """标记 Item 处理完成。

        如果处理期间有 add() 请求（dirty 标记），
        自动将 Item 重新加入 active_q，确保不丢失更新。

        注意：队列已关闭（close() 后）时不再重入队，
        处理期间的更新会被丢弃，调用方需自行处理。
        """
        with self._cond:
            self._done_locked(item)

    def delete(self, key: str) -> None:
        """从队列中彻底删除 Item。

        覆盖 active_q、in_flight、dirty 三个状态。
        """
        with self._cond:
            self._delete_locked(key)

    def close(self) -> None:
        """关闭队列，唤醒所有阻塞在 get() 的线程。"""
        with self._cond:
            self._closed = True
            self._cond.notify_all()

    # ════════════════════════════════════════════════════════════════
    # 查询接口
    # ════════════════════════════════════════════════════════════════

    def size(self) -> int:
        """返回当前待处理 Item 数量（不含 in_flight）。"""
        with self._cond:
            return self._active_q.size()

    def pending(self) -> List[Item]:
        """返回所有待处理 Item 的列表快照（不含 in_flight）。

        注意：返回顺序为堆内部存储顺序，不代表优先级或入队顺序。
        """
        with self._cond:
            return self._active_q.list()

    def is_full(self) -> bool:
        """判断 active_q 容量是否已满（backoff_q 容量独立统计）。"""
        with self._cond:
            return self._is_full_locked()

    # ════════════════════════════════════════════════════════════════
    # Template Methods（子类可重写以扩展行为）
    #
    # 调用者必须持有 self._cond。
    # ════════════════════════════════════════════════════════════════

    def _add_locked(self, item: Item) -> bool:
        """add() 的核心逻辑，调用者必须持有 self._cond。"""
        if self._closed:
            return False

        # 已在 active_q 中：更新优先级，保留 FIFO 顺序
        existing = self._active_q.get_by_key(item.key)
        if existing is not None:
            item._sequence = existing._sequence
            self._active_q.push(item)
            self._cond.notify()
            return True

        # 正在处理中：标记 dirty（不检查容量，重入队时豁免，保证更新不丢失）
        if item.key in self._in_flight:
            self._dirty.add(item.key)
            self._in_flight[item.key].priority = item.priority
            return True

        # 容量检查
        if self._is_full_locked():
            return False

        # 全新 Item
        item._sequence = self._next_sequence()
        self._active_q.push(item)
        self._cond.notify()
        return True

    def _get_locked(self) -> Optional[Item]:
        """get() 的核心逻辑，调用者必须持有 self._cond。"""
        while self._active_q.size() == 0:
            if self._closed:
                return None
            self._cond.wait()

        if self._closed:
            return None

        return self._pop_active_locked()

    def _done_locked(self, item: Item) -> None:
        """done() 的核心逻辑，调用者必须持有 self._cond。"""
        if item.key not in self._in_flight:
            return

        item_latest = self._in_flight.pop(item.key)

        # 已关闭：清理状态，不再重入队（关闭后 get() 不再派发任务）
        if self._closed:
            self._dirty.discard(item.key)
            return

        if item.key in self._dirty:
            self._dirty.discard(item.key)
            # dirty 重入队豁免容量检查：处理期间新到的更新不能丢失
            self._active_q.push(item_latest)
            self._cond.notify()

    def _delete_locked(self, key: str) -> None:
        """delete() 的核心逻辑，调用者必须持有 self._cond。"""
        dummy = Item(key)
        self._active_q.delete(dummy)
        self._in_flight.pop(key, None)
        self._dirty.discard(key)

    def _is_full_locked(self) -> bool:
        return self._max_size > 0 and self._active_q.size() >= self._max_size


    # ════════════════════════════════════════════════════════════════
    # 内部辅助方法
    # ════════════════════════════════════════════════════════════════

    def _next_sequence(self) -> int:
        """自增序列号，保证同优先级 FIFO。"""
        self._sequence += 1
        return self._sequence

    def _pop_active_locked(self) -> Item:
        """从 active_q 弹出堆顶并标记 in_flight。"""
        item = self._active_q.pop()
        self._in_flight[item.key] = item

        # 还有剩余时唤醒其他 Worker
        if self._active_q.size() > 0:
            self._cond.notify()

        return item

    @staticmethod
    def _priority_less(a: Item, b: Item) -> bool:
        """优先级堆的比较函数。

        大顶堆 + 同优先级 FIFO：
        1. 优先级高的优先（a.priority > b.priority）
        2. 同优先级时，先入队的优先（a._sequence < b._sequence）
        """
        if a.priority != b.priority:
            return a.priority > b.priority
        return a._sequence < b._sequence


# =============================================================================
# WorkQueue
# =============================================================================

class WorkQueue(PriorityQueue):
    """优先级队列 + 指数退避。

    继承 PriorityQueue，扩展退避能力：
    - 处理失败时进入退避队列，按指数退避
    - get() 自动将到期 Item 从退避队列移回活跃队列
    - add() 检测到 Item 在退避队列中时，取消退避移回活跃队列
    - 退避时长封顶，不作无限制增长
    - add_backoff() 在 backoff_q 满时返回 False，且保持 in_flight/dirty 不变
    - close() 后 done()/add_backoff() 只做状态清理，不再重入队

    通过重写 Template Method（_add_locked、_get_locked 等）扩展行为，
    不修改父类代码。

    使用示例：
        q = WorkQueue(
            max_size=640,
            backoff_initial=1.0,
            backoff_max=300.0,
            backoff_factor=2.0,
        )
        q.add(BackoffItem("job-1", priority=10))
        item = q.get()
        # ... 处理失败
        q.add_backoff(item)       # 进入退避队列
    """

    def __init__(
        self,
        max_size: int = 0,
        backoff_initial: float = 1.0,
        backoff_max: float = 300.0,
        backoff_factor: float = 2.0,
    ) -> None:
        super().__init__(max_size)

        # ── 退避队列 ──────────────────────────────────────────────
        # 小顶堆：退避到期时间最早的在前
        self._backoff_q = Heap(
            key_func=lambda item: item.key,
            less_func=self._backoff_less,
        )

        # ── 退避参数 ──────────────────────────────────────────────
        self._backoff_initial = backoff_initial   # 首次退避时长（秒）
        self._backoff_max = backoff_max           # 最大退避时长（秒）
        self._backoff_factor = backoff_factor     # 退避指数因子

        # ── 退避队列容量（独立于 active_q） ─────────────────────────
        self._backoff_max_size: int = max_size

        # ── 退避达到封顶所需的步数 ──────────────────────────────────────
        if self._backoff_factor <= 1:
            raise ValueError(
                f"backoff_factor must be greater than 1, but got {backoff_factor}"
            )

        if self._backoff_initial <= 0:
            raise ValueError(
                f"backoff_initial must be greater than 0, but got {backoff_initial}"
            )

        if self._backoff_max < self._backoff_initial:
            raise ValueError(
                f"backoff_max must be greater than backoff_initial, "
                f"but got backoff_max={backoff_max}, backoff_initial={backoff_initial}"
            )

        self._max_retry_count: int = math.ceil(
            math.log(self._backoff_max / self._backoff_initial, self._backoff_factor)
        ) + 1  # _retry_count 饱和于此值 - 1（封顶分支不自增）

    # ════════════════════════════════════════════════════════════════
    # 新增：退避相关接口
    # ════════════════════════════════════════════════════════════════

    def add_backoff(self, item: BackoffItem) -> bool:
        """处理失败，进入退避队列。

        计算指数退避时长，将 Item 移入 backoff_q。
        退避到期后，get() 中的 _flush_backoff() 自动将其移回 active_q。

        此方法同时处理"done"语义：自动从 in_flight 移除，
        调用方不需要再调用 done()。

        Args:
            item: 需要进入退避的 Item，必须是 BackoffItem 实例；
                  建议传 get() 返回的对象（仅使用其 key，优先级以
                  in_flight 中的最新值为准）。

        Returns:
            True 表示成功进入退避队列；False 表示队列已关闭（此时自动清理
            in_flight/dirty）、Item 不在 in_flight 中、或 backoff_q 已满
            （后两种情况保持 in_flight/dirty 不变，可稍后重试或调用
            done()/delete() 收尾）。
        """
        with self._cond:
            return self._add_backoff_locked(item)

    def stats(self) -> Dict[str, int]:
        """返回各队列深度详情，用于监控。"""
        with self._cond:
            return {
                "active": self._active_q.size(),
                "backoff": self._backoff_q.size(),
                "in_flight": len(self._in_flight),
            }

    # ════════════════════════════════════════════════════════════════
    # 重写公共接口
    # ════════════════════════════════════════════════════════════════

    def add(self, item: BackoffItem) -> bool:
        """添加或更新 Item（扩展：支持取消退避）。

        语义：
        - Item 在 backoff_q 中且 active_q 未满：取消退避，立即移回 active_q
        - Item 在 backoff_q 中但 active_q 已满：仅更新优先级并保留原退避计划，
          返回 True（更新已生效，但不会提前唤醒）
        - 其他情况：与 PriorityQueue.add 一致

        注意：返回 True 表示更新已接受，但不保证 Item 已移入 active_q
        （active_q 满时仍留在退避队列）。
        """
        with self._cond:
            return self._add_locked(item)

    def get(self) -> Optional[BackoffItem]:
        """阻塞获取下一个待处理的 Item（扩展：支持退避到期自动迁移）。"""
        with self._cond:
            return self._get_locked()

    def done(self, item: BackoffItem) -> None:
        """标记 Item 处理完成（扩展：退避中的 Item 不重新入队）。

        注意：队列已关闭（close() 后）时不再重入队，
        处理期间的更新会被丢弃，调用方需自行处理。
        """
        with self._cond:
            self._done_locked(item)

    def delete(self, key: str) -> None:
        """从所有队列中彻底删除 Item（扩展：同时清理 backoff_q）。"""
        with self._cond:
            self._delete_locked(key)

    def size(self) -> int:
        """返回待处理 Item 总数（active_q + backoff_q，不含 in_flight）。"""
        with self._cond:
            return self._active_q.size() + self._backoff_q.size()

    def pending(self) -> List[BackoffItem]:
        """返回所有待处理 Item（含 backoff_q 中的 Item）。

        注意：返回顺序为堆内部存储顺序，不代表优先级或入队顺序。
        """
        with self._cond:
            return self._active_q.list() + self._backoff_q.list()

    # ════════════════════════════════════════════════════════════════
    # 重写 Template Methods
    # ════════════════════════════════════════════════════════════════

    def _add_locked(self, item: BackoffItem) -> bool:
        """add() 核心逻辑（扩展：取消退避）。

        调用者必须持有 self._cond。
        """
        if self._closed:
            return False

        # ── 在 backoff_q 中 ────────────────────────────────────────
        existing_backoff = self._backoff_q.get_by_key(item.key)
        if existing_backoff is not None:

            # active_q 已满：无法取消退避，仅更新优先级
            # （退避到期后按新优先级调度，push 不需要：堆序只依赖 _backoff_time）
            if self._is_active_full_locked():
                existing_backoff.priority = item.priority
                return True

            # 取消退避：保留原 FIFO 序列号，重置退避状态，移回 active_q
            item._sequence = existing_backoff._sequence
            self._backoff_q.delete(item)
            item._backoff_time = 0.0
            item._retry_count = 0
            self._active_q.push(item)
            self._cond.notify()
            return True

        # ── 全新 Item（不在 active/in_flight/backoff 中）：重置退避状态，
        #    支持调用方复用 BackoffItem 对象 ────────────────────────
        if (
            self._active_q.get_by_key(item.key) is None
            and item.key not in self._in_flight
        ):
            item._backoff_time = 0.0
            item._retry_count = 0

        # ── 其他情况：委托给父类 ─────────────────────────────────
        return super()._add_locked(item)

    def _get_locked(self) -> Optional[BackoffItem]:
        """get() 核心逻辑（扩展：退避到期自动迁移）。

        调用者必须持有 self._cond。
        """
        while True:
            if self._closed:
                return None

            # 将到期 Item 从 backoff_q 移回 active_q
            self._flush_backoff()

            # 从 active_q 取出最高优先级的 Item
            if self._active_q.size() > 0:
                return self._pop_active_locked()

            # active_q 为空：以 backoff_q 最早到期时间为超时等待
            head = self._backoff_q.peek()
            if head is not None:
                wait = head._backoff_time - time.monotonic()
                self._cond.wait(timeout=max(wait, 0.0))
            else:
                self._cond.wait()

    def _done_locked(self, item: BackoffItem) -> None:
        """done() 核心逻辑（扩展：退避中不重新入队）。

        调用者必须持有 self._cond。
        """
        if item.key not in self._in_flight:
            return

        item_latest = self._in_flight.pop(item.key)

        # 已关闭：清理状态，不再重入队
        if self._closed:
            self._dirty.discard(item.key)
            return

        if item.key in self._dirty:
            self._dirty.discard(item.key)
            # dirty 重入队豁免容量检查：处理期间新到的更新不能丢失；
            # 保留 _retry_count，失败历史沿重试链延续（指数退避不因更新而重置）
            self._active_q.push(item_latest)
            self._cond.notify()
            return

        # 成功完成（无 pending 更新）：重置退避状态，允许调用方复用 Item 对象
        item_latest._backoff_time = 0.0
        item_latest._retry_count = 0

    def _delete_locked(self, key: str) -> None:
        """delete() 核心逻辑（扩展：同时清理 backoff_q）。

        调用者必须持有 self._cond。
        """
        dummy = BackoffItem(key)
        self._active_q.delete(dummy)
        self._backoff_q.delete(dummy)
        self._in_flight.pop(key, None)
        self._dirty.discard(key)
        # 唤醒等待中的 get()，避免其睡到被删项原定的到期时间
        self._cond.notify()

    # ════════════════════════════════════════════════════════════════
    # 新增内部方法：退避逻辑
    # ════════════════════════════════════════════════════════════════

    def _is_active_full_locked(self) -> bool:
        return super()._is_full_locked()

    def _add_backoff_locked(self, item: BackoffItem) -> bool:
        """add_backoff() 核心逻辑，调用者必须持有 self._cond。

        成功进入退避时自动弹出 in_flight 并清理 dirty（自动 done 语义）；
        已关闭时同样清理状态后返回 False；backoff_q 满时保持
        in_flight/dirty 不变，由调用方决定重试或收尾。
        """
        # 1. 已关闭 → 清理 in_flight/dirty 后返回 False（Item 不进入退避）
        if self._closed:
            if item.key in self._in_flight:
                self._in_flight.pop(item.key)
                self._dirty.discard(item.key)
            return False

        # 2. 只有 in_flight 中的 Item 才能进入退避
        if item.key not in self._in_flight:
            return False

        # 3. backoff_q 满时拒绝：保持 in_flight/dirty 原样，
        #    调用方可以稍后重试或调用 done()/delete() 收尾
        if self._backoff_max_size > 0 and self._backoff_q.size() >= self._backoff_max_size:
            return False

        # 4. 弹出 in_flight 并清理 dirty（自动 done 语义）
        item_latest = self._in_flight.pop(item.key)
        self._dirty.discard(item.key)

        # 5. 计算退避时长并写入 in_flight 弹出的同一对象；
        #    优先级沿用 in_flight 对象（已包含处理期间 add() 的最新更新），
        #    不采用传入对象的 priority
        item_latest._backoff_time = time.monotonic() + self._calc_backoff(item_latest)

        # 6. push 到 backoff_q，notify，返回 True
        self._backoff_q.push(item_latest)
        self._cond.notify()
        return True

    @staticmethod
    def _backoff_less(a: BackoffItem, b: BackoffItem) -> bool:
        """退避队列的比较函数：按退避到期时间升序（小顶堆）。"""
        return a._backoff_time < b._backoff_time

    def _calc_backoff(self, item: BackoffItem) -> float:
        """计算指数退避时长。

        公式：min(initial * factor^(retry_count-1), max_backoff)。

        退避时长封顶于 max_backoff：一旦指数增长已必然达到封顶值，
        直接返回 max_backoff，避免 retry_count 无限增长导致浮点指数
        溢出（OverflowError）。

        行为说明：
        - 封顶分支不自增 retry_count；
        - 重试间隔在达到 max_backoff 后保持恒定；_retry_count 只在
          成功完成、取消退避或全新 add() 时归零。
        """
        n = item._retry_count + 1

        if n >= self._max_retry_count:
            return self._backoff_max

        item._retry_count = n

        # n < max_retry_count 时，initial * factor^(n-1) 数学上必然小于
        # max_backoff（封顶已由上面的分支保证），无需再 min 截断。
        return self._backoff_initial * (self._backoff_factor ** (n - 1))

    def _flush_backoff(self, max_flush: int = 32) -> None:
        """将 backoff_q 中到期的 Item 移回 active_q。

        调用者必须持有 self._cond。
        单次迁移上限 32 个，避免持锁过久。
        """
        if self._is_active_full_locked():
            return

        now = time.monotonic()
        head = self._backoff_q.peek()
        while not self._is_active_full_locked() and head is not None \
            and head._backoff_time <= now and 0 < max_flush:

            item = self._backoff_q.pop()
            item._backoff_time = 0.0
            # 保留 _retry_count：指数退避阶梯跨 flush 延续
            self._active_q.push(item)
            head = self._backoff_q.peek()
            max_flush -= 1
