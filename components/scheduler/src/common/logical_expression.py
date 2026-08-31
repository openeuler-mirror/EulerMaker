'''逻辑运算表达式

基于expression设计模式实现对大于、小于、等于、存在的表达式求值。

表达式操作有:
    Lt: 小于
    Gt: 大于
    Equal: 等于
    Exists: 存在
'''
from __future__ import annotations
from abc import ABC, abstractmethod
from typing import Any, List


class LogicExpression(ABC):
    @abstractmethod
    def evaluate(self) -> Any:
        ...


class AndExpression(LogicExpression):
    '''与表达式'''

    def __init__(self):
        self._items: List[LogicExpression] = []

    def add(self, item: LogicExpression):
        self._items.append(item)

    def evaluate(self) -> bool:
        return all(item.evaluate() for item in self._items)

    def __eq__(self, other: object) -> bool:
        if not isinstance(other, AndExpression):
            return NotImplemented
        return self._items == other._items

    def __str__(self) -> str:
        return f"AndExpression({', '.join([str(item) for item in self._items])})"


class NumberValExpression(LogicExpression):
    '''数值表达式'''

    def __init__(self, value: int | float):
        self._value: int | float = value

    def evaluate(self) -> int | float:
        return self._value

    def __eq__(self, other: object) -> bool:
        if not isinstance(other, NumberValExpression):
            return NotImplemented
        return self._value == other._value

    def __str__(self) -> str:
        return f"NumberValExpression({self._value})"


class StrValExpression(LogicExpression):
    '''字符串表达式'''

    def __init__(self, value: str):
        self._value: str = value

    def evaluate(self) -> str:
        return self._value

    def __eq__(self, other: object) -> bool:
        if not isinstance(other, StrValExpression):
            return NotImplemented
        return self._value == other._value

    def __str__(self) -> str:
        return f"StrValExpression({self._value})"


class _BinaryExpression(LogicExpression):
    '''二元比较表达式基类'''

    def __init__(self, left: LogicExpression, right: LogicExpression):
        self._left: LogicExpression = left
        self._right: LogicExpression = right

    @abstractmethod
    def _comparison(self, left_val: Any, right_val: Any) -> bool:
        ...

    def evaluate(self) -> bool:
        # 操作数只求值一次, 再传给 _comparison, 保证比较看到的是同一组值。
        # None 的处理下放给子类: 排序(Gt/Lt)对 None 无定义须返回 False,
        # 而相等比较(Equal)应允许 None 作为合法值参与(None == None 为 True),
        # 与 NoneExpression 的类型模型保持一致。
        left_val = self._left.evaluate()
        right_val = self._right.evaluate()
        return self._comparison(left_val, right_val)

    def __eq__(self, other: object) -> bool:
        if not isinstance(other, type(self)):
            return NotImplemented
        return self._left == other._left and self._right == other._right

    def __str__(self) -> str:
        return f"{type(self).__name__}({self._left}, {self._right})"


class GtExpression(_BinaryExpression):
    '''大于表达式
    检查左操作数是否大于右操作数。

    支持的操作数类型:
        - 左操作数: 任何可排序类型 (int, float, str, etc.)
        - 右操作数: 任何可排序类型 (int, float, str, etc.)
    '''

    def _comparison(self, left_val: Any, right_val: Any) -> bool:
        # None 无排序语义, 一律 False
        if left_val is None or right_val is None:
            return False
        try:
            return left_val > right_val
        except TypeError:
            return False


class LtExpression(_BinaryExpression):
    '''小于表达式
    检查左操作数是否小于右操作数。

    支持的操作数类型:
        - 左操作数: 任何可排序类型 (int, float, str, etc.)
        - 右操作数: 任何可排序类型 (int, float, str, etc.)
    '''

    def _comparison(self, left_val: Any, right_val: Any) -> bool:
        # None 无排序语义, 一律 False
        if left_val is None or right_val is None:
            return False
        try:
            return left_val < right_val
        except TypeError:
            return False


class EqualExpression(_BinaryExpression):
    '''等于表达式'''

    def _comparison(self, left_val: Any, right_val: Any) -> bool:
        return left_val == right_val


class ExistsExpression(_BinaryExpression):
    '''存在表达式
    检查左操作数是否存在于右操作数中。

    支持的操作数类型:
        - 左操作数: 任何可哈希类型 (int, str, tuple, etc.)
        - 右操作数: 任何可迭代类型 (list, tuple, set, dict, str, etc.)
    '''

    def _comparison(self, left_val: Any, right_val: Any) -> bool:
        # None 无存在语义, 一律 False
        if left_val is None or right_val is None:
            return False

        if not isinstance(right_val, (list, tuple, set, dict, str)):
            return False

        try:
            return left_val in right_val
        except TypeError:
            return False


class TrueExpression(LogicExpression):
    '''True表达式'''

    def evaluate(self) -> bool:
        return True

    def __eq__(self, other: object) -> bool:
        return isinstance(other, TrueExpression)

    def __str__(self) -> str:
        return "TrueExpression()"


class FalseExpression(LogicExpression):
    '''False表达式'''

    def evaluate(self) -> bool:
        return False

    def __eq__(self, other: object) -> bool:
        return isinstance(other, FalseExpression)

    def __str__(self) -> str:
        return "FalseExpression()"


class NoneExpression(LogicExpression):
    '''None表达式'''

    def evaluate(self) -> None:
        return None

    def __eq__(self, other: object) -> bool:
        return isinstance(other, NoneExpression)

    def __str__(self) -> str:
        return "NoneExpression()"


class AnyExpression(LogicExpression):
    '''Any表达式'''

    def __init__(self, value: Any):
        self._value: Any = value

    def evaluate(self) -> Any:
        return self._value

    def __eq__(self, other: object) -> bool:
        if not isinstance(other, AnyExpression):
            return NotImplemented
        return self._value == other._value

    def __str__(self) -> str:
        return f"AnyExpression({self._value})"


# 操作符到表达式的映射
_OPERATORS_MAP_EXPRESSION = {
    'Gt': GtExpression,
    'Lt': LtExpression,
    'Equal': EqualExpression,
    'Exists': ExistsExpression,
}


def operator_to_expression(operator: str, left: LogicExpression, right: LogicExpression) -> LogicExpression:
    '''根据操作符生成表达式。不支持的运算符抛出 ValueError，避免未知运算符导致 filter 静默通过。'''
    if operator not in _OPERATORS_MAP_EXPRESSION:
        raise ValueError(
            f"Unknown operator: {operator!r}. "
            f"Supported operators: {list(_OPERATORS_MAP_EXPRESSION)}"
        )
    return _OPERATORS_MAP_EXPRESSION[operator](left, right)


def value_to_expression(value: Any) -> LogicExpression:
    """
    将值转换为表达式。
    如果 value 是 int、float、bool、None 或 str 类型，则返回对应的表达式。
    否则返回 AnyExpression。

    Args:
        value (Any): 要转换的值

    Returns:
        转换后的表达式
    """
    if isinstance(value, bool):
        return TrueExpression() if value else FalseExpression()

    if value is None:
        return NoneExpression()

    if isinstance(value, (int, float)):
        return NumberValExpression(value)

    if isinstance(value, str):
        return StrValExpression(value)

    return AnyExpression(value)
