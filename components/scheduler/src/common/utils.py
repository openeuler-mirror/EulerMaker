
"""工具函数模块

这是一个工具函数模块, 包含了一些常用的工具函数。

包含以下函数:
- find_index_value: 根据索引路径获取对象的属性值函数
- key_func: 对象的唯一标识函数
"""
from __future__ import annotations
import math
import sys
import time
from typing import Any, Callable, List, Optional

_SENTINEL = object()


def _index_obj_value(obj: Any, attr_name: str, default_value: Any = None) -> Any:
    if isinstance(obj, str):
        return default_value
    # 只调用一次 getattr: 既避免重复访问的开销, 也避免动态 __getattr__/property
    # 在两次访问间返回不同值(或第二次访问时状态已变化)
    value = getattr(obj, attr_name, _SENTINEL)
    return default_value if value is _SENTINEL else value


def _index_dict_value(obj: Any, attr_name: str, default_value: Any = None) -> Any:
    if isinstance(obj, dict) and attr_name in obj:
        return obj[attr_name]
    return default_value


def _index_value(obj: Any,
                 index_path: List[str],
                 max_depth: int = 7,
                 depth: int = 0,
                 default_value: Any = None) -> Any:
    if obj is None or not index_path or depth > max_depth or depth >= len(index_path):
        return default_value

    attr_name = index_path[depth]
    attr_value = _index_obj_value(obj, attr_name, default_value) if not isinstance(obj, dict) \
        else _index_dict_value(obj, attr_name, default_value)

    if depth + 1 < len(index_path):
        return _index_value(attr_value, index_path, max_depth, depth + 1, default_value)

    return attr_value


def find_index_value(obj: Any, index_path: List[str], max_depth: int = 7, default_value: Any = None) -> Any:
    return _index_value(obj, index_path, max_depth, default_value=default_value)


def key_func(obj: Any, index_path=None) -> str:
    '''
    对象的唯一标识, 默认: metadata.name
    '''
    if index_path is None:
        index_path = ['metadata', 'name']

    name = find_index_value(obj, index_path)
    if name is None:
        raise ValueError("object does not have metadata.name attribute")

    namespace = find_index_value(obj, ['metadata', 'namespace'])

    return f'{namespace}/{name}' if namespace else name


def extract_name_from_key(key: str) -> str:
    '''
    从key中提取name
    '''
    if not key or '/' not in key:
        return key

    return key.split('/')[-1]


def uid_func(obj: Any, index_path=None) -> str:
    '''
    对象的唯一标识, 默认: metadata.uid
    '''
    if index_path is None:
        index_path = ['metadata', 'uid']
    return find_index_value(obj, index_path)


def _set_value(obj: Any,
               index_path: List[str],
               value: Any,
               depth: int = 0) -> None:
    if obj is None or not index_path or depth >= len(index_path):
        return

    attr_name = index_path[depth]

    if depth + 1 < len(index_path):
        if isinstance(obj, dict):
            if attr_name not in obj:
                raise KeyError(
                    f"key {attr_name!r} not found at index_path[{depth}]={attr_name!r}")
            child = obj[attr_name]
        else:
            if not hasattr(obj, attr_name):
                raise AttributeError(
                    f"'{type(obj).__name__}' object has no attribute '{attr_name}' "
                    f"at index_path[{depth}]={attr_name!r}")
            child = getattr(obj, attr_name)
        _set_value(child, index_path, value, depth + 1)
        return

    if isinstance(obj, dict):
        obj[attr_name] = value
    else:
        setattr(obj, attr_name, value)


def set_index_value(obj: Any,
                    index_path: List[str],
                    value: Any) -> None:
    _set_value(obj, index_path, value, depth=0)


def _validate_numeric_unit_input(value, label: str = "value") -> str:
    '''校验数值型单位输入: 提取规范化的数字字符串, 非法时抛出 ValueError。'''
    s = str(value).strip()
    if not s:
        raise ValueError(
            f"Invalid {label}: {value!r}. Expected a numeric value with optional unit suffix.")
    try:
        number = float(s)
    except ValueError as exc:
        raise ValueError(
            f"Invalid {label}: {value!r}. Expected a numeric value with optional unit suffix."
        ) from exc
    if not math.isfinite(number):
        raise ValueError(
            f"Invalid {label}: {value!r}. Expected a finite numeric value.")
    return s


# 存储单位后缀 -> 以 1024 为底的指数(结果单位: Byte)。
# 元组顺序: 后缀长的在前, 避免 'b' 误匹配 'kb'/'mb'/'gb'/'tb' 的尾部。
_BYTE_UNIT_EXPONENTS = (
    ('ti', 4), ('tb', 4),
    ('gi', 3), ('gb', 3),
    ('mi', 2), ('mb', 2),
    ('ki', 1), ('kb', 1),
    ('b', 0),
)


# 字节单位进制: KiB/MiB/GiB/TiB 按 1024 换算
_BYTE_UNIT_BASE = 1024


def store_unit_to_byte(size: int | str) -> int:
    ''' 将带单位的长度转换成不带单位的数值(转换后的单位: Byte)

    Args:
        size: 
            - 类型 str: 带单位的长度, 如: 10Gi, 100Mi, 10Ki
            - 类型 int: 不带单位的长度(默认单位: Byte), 如: 102400
    Return:
        result: 不带单位的数值 (转换后的单位: Byte)
    '''
    if not isinstance(size, str):
        return int(float(_validate_numeric_unit_input(size, "unit value")))

    size_lower = size.strip().lower()
    for suffix, exponent in _BYTE_UNIT_EXPONENTS:
        if size_lower.endswith(suffix):
            number = _validate_numeric_unit_input(
                size_lower[: -len(suffix)], "unit value")
            return int(float(number) * (_BYTE_UNIT_BASE ** exponent))

    return int(float(_validate_numeric_unit_input(size, "unit value")))


# CPU 核心数量单位后缀(当作"核", 乘以 _CORE_UNIT_BASE): 后缀长的在前
_CORE_UNIT_SUFFIXES = ('core', 'c', 'u')

# 毫核后缀: 输入已是毫核, 原样返回(不乘 1000)
_CORE_UNIT_BASE_SUFFIX = 'm'

# CPU 核心数量单位进制: 1000 毫核 = 1 核
_CORE_UNIT_BASE = 1000


def core_unit_to_quantity(unit: int | str) -> int:
    '''统一转换为毫核: 1000 毫核 = 1 核
    不带单位默认单位为核

    剥离 c/C/u/U/Core/core 后缀(当作"核", 乘以 1000)
    m 后缀表示已是毫核, 原样返回。

    如: "1000m" -> 1000, "1000" -> 1000000, "1000c" -> 1000000, "0.5c" -> 500
    '''
    if isinstance(unit, str):
        unit_lower = unit.strip().lower()
        if unit_lower.endswith(_CORE_UNIT_BASE_SUFFIX):
            return int(float(_validate_numeric_unit_input(
                unit_lower[: -len(_CORE_UNIT_BASE_SUFFIX)], "core unit value")))
        for suffix in _CORE_UNIT_SUFFIXES:
            if unit_lower.endswith(suffix):
                unit = unit_lower[: -len(suffix)]
                break
        else:
            unit = unit_lower

    value = float(_validate_numeric_unit_input(unit, "core unit value"))
    return int(value * _CORE_UNIT_BASE)


def _identity(x):
    return x


_RESOURCE_METRIC_FUNCTIONS = {
    "cpu": core_unit_to_quantity,
    "memory": store_unit_to_byte,
    "disk": store_unit_to_byte,
    "gpu": core_unit_to_quantity,
    "vram": store_unit_to_byte,
    "ephemeral_storage": store_unit_to_byte,
    "gpu_model": _identity,
}


def find_resource_metric_func(metric: str) -> Callable[[int | str], Any]:
    if metric in _RESOURCE_METRIC_FUNCTIONS:
        return _RESOURCE_METRIC_FUNCTIONS[metric]
    return _identity


def format_time_now() -> str:
    '''
    获取格式化当前时间
    返回时间格式为: "YYYY-MM-DDTHH:MM:SSZ"
    例子："2026-06-09T10:00:00Z"
    '''

    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def size_of_object(obj: Any, _seen: Optional[set] = None) -> int:
    '''
    递归获取对象的大小, 单位为字节

    计算说明：
      - 支持递归计算嵌套对象的大小
      - 支持递归计算Class对象和其属性的大小
      - 自动处理循环引用避免无限递归
    '''
    if _seen is None:
        _seen = set()

    obj_id = id(obj)
    if obj_id in _seen:
        return 0
    _seen.add(obj_id)

    size = sys.getsizeof(obj)

    if isinstance(obj, dict):
        size += sum(size_of_object(k, _seen) + size_of_object(v, _seen) for k, v in obj.items())
    elif isinstance(obj, (list, tuple, set, frozenset)):
        size += sum(size_of_object(item, _seen) for item in obj)
    if hasattr(obj, '__dict__'):
        size += size_of_object(obj.__dict__, _seen)

    if hasattr(obj, '__slots__'):
        for slot in obj.__slots__:
            if hasattr(obj, slot):
                size += size_of_object(getattr(obj, slot), _seen)

    return size
