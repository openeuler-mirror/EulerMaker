from typing import Any

from common import key_func
from .cache_queue import KeyFunc


class MetaNamespaceKeyFunc(KeyFunc):
    '''
    元命名空间键函数类, 用于将资源对象转换为元命名空间键。
    元命名空间键的格式为: {namespace}/{name}
    如果资源对象没有命名空间, 则键的格式为: {name}
    '''

    def key(self, obj: Any) -> str:
        if not obj or isinstance(obj, str):
            raise ValueError("Cannot extract key from empty object or string object")

        name = key_func(obj, ['metadata', 'name'])
        namespace = key_func(obj, ['metadata', 'namespace'])

        if name:
            return f'{namespace}/{name}' if namespace else name

        raise ValueError("Spec object does not have metadata.name attribute")
