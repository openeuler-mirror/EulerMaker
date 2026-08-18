"""缓存队列模块

这是一个缓存队列模块, 实现了队列的基本操作, 包括添加、更新、删除、获取、列表等。同时兼具队列的特性, 可以用于存储增量事件。

提供缓存队列的实现, 包括 DeltaFIFO 队列。

DeltaFIFO 队列用于缓存资源类型的增量事件, 同时兼具队列的特性和事件快照功能。
"""
from .delta_fifo import DeltaFIFO, Delta, QueueClosedError
from .work_queue import Item, BackoffItem, PriorityQueue, WorkQueue

__all__ = [
    "DeltaFIFO",
    "Delta",
    "QueueClosedError",

    "Item",
    "BackoffItem",
    "PriorityQueue",
    "WorkQueue",
]
