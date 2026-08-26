"""缓存队列定义模块

定义了缓存队列的基本接口和实现类。

包含:
    - Store: 缓存存储类, 定义了缓存对象的添加、更新、删除、获取和列表操作。
    - Queue: 缓存队列类, 定义了缓存对象的弹出操作。
    - KeyFunc: 键函数类, 定义了将对象转换为键的函数。
"""

from abc import ABC, abstractmethod
from typing import Any, List


class Store(ABC):
    '''
    缓存存储类
    '''

    @abstractmethod
    def add(self, obj: Any):
        '''
        添加缓存对象
        '''
        ...

    @abstractmethod
    def update(self, obj: Any):
        '''
        更新缓存对象
        '''
        ...

    @abstractmethod
    def delete(self, obj: Any):
        '''
        删除缓存对象
        '''
        ...

    @abstractmethod
    def get(self, obj: Any) -> Any:
        '''
        获取缓存对象
        '''
        ...

    @abstractmethod
    def list(self) -> List[Any]:
        '''
        获取所有缓存对象
        '''
        ...

    @abstractmethod
    def replace(self, obj_list: List[Any], resource_version: str):
        '''
        全量同步缓存对象
        '''
        ...


class Queue(Store):

    @abstractmethod
    def pop(self) -> Any:
        '''
        弹出队列中的对象
        '''
        ...


class KeyFunc(ABC):
    '''
    键函数类
    '''

    @abstractmethod
    def key(self, obj: Any) -> str:
        '''
        提取对象的 key
        '''
        ...
