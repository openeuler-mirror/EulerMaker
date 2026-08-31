"""事件模型

这是事件模型, 用于表示资源事件通知。

包含：
    - ResourceEvent: 资源事件
        - event_type: 事件类型
        - object: 关联的 Job 或 Runner 对象

    - Notify: 事件通知基类
        - new_obj: 新对象

    - NotifyUpdate: 更新事件
        - old_obj: 旧对象

    - NotifyDelete: 删除事件

    - NotifyAdd: 添加事件
"""
from __future__ import annotations
from typing import Optional, Any
from .types import EventType
from pydantic import ConfigDict, Field, BaseModel
from dataclasses import dataclass


class ResourceEvent(BaseModel):
    """
    资源事件基类
    """
    # 支持字段名称和别名初始化配置
    model_config = ConfigDict(populate_by_name=True)

    event_type: EventType = Field(alias="type")
    object: Optional[Any] = Field(default=None)


@dataclass(slots=True)
class Notify:
    """
    事件通知基类
    """
    new_obj: Optional[Any] = None


@dataclass(slots=True)
class NotifyUpdate(Notify):
    """
    更新事件
    """
    old_obj: Optional[Any] = None


@dataclass(slots=True)
class NotifyDelete(Notify):
    """删除事件"""
    pass


@dataclass(slots=True)
class NotifyAdd(Notify):
    """添加事件"""
    pass
