"""
通用堆（优先队列）实现。

提供基于 key 的 O(1) 查找和更新操作，支持自定义排序规则。
参考 Kubernetes scheduler 的 heap 实现和 Go 标准库 container/heap 的算法，
不提供并发安全保证，同步应由外部调用方处理。

使用方式:
    heap = Heap(
        key_func=lambda obj: obj.name,
        less_func=lambda a, b: a.priority < b.priority,
    )
    heap.push(item)
    top = heap.pop()
"""

from typing import Any, Callable, Dict, Hashable, List, Optional


# =============================================================================
# 堆内部包装对象
# =============================================================================

class _HeapItem:
    """堆内部包装对象，将 obj 与 key 绑定。"""

    __slots__ = ('obj', 'key')

    def __init__(self, obj: Any, key: Hashable) -> None:
        self.obj = obj
        self.key = key


# =============================================================================
# 堆操作函数（对应 Go container/heap 包）
#
# 这些函数操作任意实现了以下协议的对象 h：
#   h._less(i, j)      — 比较索引 i 和 j 处的元素，返回 True 表示 i 应排在 j 前
#   h._swap(i, j)      — 交换索引 i 和 j 处的元素
#   h._len()            — 返回堆中元素数量
#   h._push_item(item)  — 将 item 追加到队列末尾（不维护堆序）
#   h._pop_item()       — 移除并返回队列末尾元素的 obj（不维护堆序）
# =============================================================================

def _heap_up(h: Any, j: int) -> None:
    """将 j 处的元素向根方向移动，恢复堆序。对应 container/heap 的 up。"""
    while j > 0:
        i = (j - 1) >> 1  # parent
        if not h._less(j, i):
            break
        h._swap(i, j)
        j = i


def _heap_down(h: Any, i0: int, n: int) -> bool:
    """将 i0 处的元素向叶子方向移动，恢复堆序。

    对应 container/heap 的 down，返回 True 表示元素发生了移动。

    Args:
        i0: 起始索引。
        n:  队列有效长度（用于 remove 时忽略已交换到尾部的元素）。
    """
    i = i0
    while True:
        j1 = (i << 1) + 1  # left child
        if j1 >= n or j1 < 0:
            break
        j = j1
        j2 = j1 + 1  # right child
        if j2 < n and h._less(j2, j1):
            j = j2
        if not h._less(j, i):
            break
        h._swap(i, j)
        i = j
    return i > i0


def heap_push(h: Any, x: Any) -> None:
    """将 x 加入堆中，O(log n)。对应 container/heap 的 Push。"""
    h._push_item(x)
    _heap_up(h, h._len() - 1)


def heap_pop(h: Any) -> Any:
    """移除并返回堆顶元素，O(log n)。对应 container/heap 的 Pop。"""
    n = h._len() - 1
    h._swap(0, n)
    _heap_down(h, 0, n)
    return h._pop_item()


def heap_remove(h: Any, i: int) -> Any:
    """移除并返回索引 i 处的元素，O(log n)。对应 container/heap 的 Remove。"""
    n = h._len() - 1
    if n != i:
        h._swap(i, n)
        if not _heap_down(h, i, n):
            _heap_up(h, i)
    return h._pop_item()


def heap_fix(h: Any, i: int) -> None:
    """修正索引 i 处元素的位置，恢复堆序。对应 container/heap 的 Fix。"""
    if not _heap_down(h, i, h._len()):
        _heap_up(h, i)


# =============================================================================
# Heap：基于 key 的优先队列
# =============================================================================

class Heap:
    """通用堆（优先队列）。

    内部使用二叉堆维护对象顺序，同时维护 key → index 映射以支持 O(1) 的
    按 key 查找、更新和删除操作。

    契约：key_func 返回的 key 必须在对象进入堆后的整个生命周期内保持稳定；
    若对象被外部修改导致 key 变化，堆的索引会损坏且无法被检测。需要修改
    key 时，应先 delete() 再 push() 新对象。

    非线程安全，同步应由外部调用方处理。

    Args:
        key_func:  从对象中提取 key 的函数，key 用于标识对象。
        less_func: 比较函数，less_func(a, b) 返回 True 表示 a 应排在 b 前面。
    """

    def __init__(
        self,
        key_func: Callable[[Any], Hashable],
        less_func: Callable[[Any, Any], bool],
    ) -> None:
        self._queue: List[_HeapItem] = []          # 堆有序的 item 列表
        self._key_index: Dict[Hashable, int] = {}  # key → 在 _queue 中的索引
        self._key_func = key_func
        self._less_func = less_func

    # ------------------------------------------------------------------
    # 堆协议（供 _heap_up / _heap_down / heap_push / heap_pop 等函数调用）
    # ------------------------------------------------------------------

    def _less(self, i: int, j: int) -> bool:
        """比较 _queue[i] 和 _queue[j] 的 obj。"""
        return self._less_func(self._queue[i].obj, self._queue[j].obj)

    def _swap(self, i: int, j: int) -> None:
        """交换 _queue[i] 和 _queue[j]，同步更新 _key_index。"""
        self._queue[i], self._queue[j] = self._queue[j], self._queue[i]
        self._key_index[self._queue[i].key] = i
        self._key_index[self._queue[j].key] = j

    def _len(self) -> int:
        """返回队列长度。"""
        return len(self._queue)

    def _push_item(self, item: _HeapItem) -> None:
        """将 item 追加到队列末尾，设置 key 映射。"""
        self._key_index[item.key] = len(self._queue)
        self._queue.append(item)

    def _pop_item(self) -> Any:
        """移除并返回队列末尾元素的 obj，清理 key 映射。"""
        if not self._queue:
            return None
        item = self._queue.pop()  # O(1) 摊还；切片复制会退化为 O(n)
        del self._key_index[item.key]
        return item.obj

    # ------------------------------------------------------------------
    # 公共接口
    # ------------------------------------------------------------------

    def push(self, obj: Any) -> None:
        """添加或更新对象。

        如果堆中已存在相同 key 的对象，则更新该对象并修正堆序；
        否则将对象加入堆中。

        对应 heap_1.go 中 Heap[T].AddOrUpdate。
        """
        key = self._key_func(obj)
        idx = self._key_index.get(key)
        if idx is not None:
            self._queue[idx].obj = obj
            heap_fix(self, idx)
        else:
            heap_push(self, _HeapItem(obj, key))

    def pop(self) -> Any:
        """弹出堆顶对象并返回。

        对应 heap_1.go 中 Heap[T].Pop。

        Raises:
            IndexError: 堆为空时抛出。
        """
        if self._len() == 0:
            raise IndexError("pop from empty heap")
        return heap_pop(self)

    def peek(self) -> Optional[Any]:
        """返回堆顶对象但不移除。

        对应 heap_1.go 中 Heap[T].Peek。
        """
        if self._queue:
            return self._queue[0].obj
        return None

    def delete(self, obj: Any) -> Optional[Any]:
        """根据 key 删除对象。

        对应 heap_1.go 中 Heap[T].Delete。
        """
        key = self._key_func(obj)
        idx = self._key_index.get(key)
        if idx is not None:
            return heap_remove(self, idx)
        return None

    def get(self, obj: Any) -> Optional[Any]:
        """根据 key 获取对象。

        对应 heap_1.go 中 Heap[T].Get。
        """
        key = self._key_func(obj)
        return self.get_by_key(key)

    def get_by_key(self, key: Hashable) -> Optional[Any]:
        """根据 key 获取对象。

        对应 heap_1.go 中 Heap[T].GetByKey。
        """
        idx = self._key_index.get(key)
        if idx is not None:
            return self._queue[idx].obj
        return None

    def has(self, obj: Any) -> bool:
        """检查对象是否在堆中。

        对应 heap_1.go 中 Heap[T].Has。
        """
        key = self._key_func(obj)
        return key in self._key_index

    def list(self) -> List[Any]:
        """返回堆中所有对象的列表。

        对应 heap_1.go 中 Heap[T].List。
        """
        return [item.obj for item in self._queue]

    def size(self) -> int:
        """返回堆中对象数量。

        对应 heap_1.go 中 Heap[T].Len。
        """
        return len(self._queue)

    def __len__(self) -> int:
        return len(self._queue)

    def __repr__(self) -> str:
        return f"Heap({[item.key for item in self._queue]!r})"
