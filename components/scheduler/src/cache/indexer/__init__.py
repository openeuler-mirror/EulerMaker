
"""索引模块

定义了索引的基本接口和实现类。

索引是一种数据结构, 用于存储和检索数据。
索引的主要作用是加速数据的查询操作, 从而提高系统的性能。

包含:
    - Indexer: 索引类, 定义了索引的添加、更新、删除、获取和列表操作。
    - IndexerOptions: 索引选项类, 定义了索引的配置选项。

索引器：
    索引器是一种用于将资源对象转换为索引键的函数。
    索引器的主要作用是将资源对象的属性值转换为索引键, 以便于快速检索和查询。

"""

from .indexer import Indexer, IndexerOptions
from .default_indexer import DefaultIndexer

__all__ = [
    "Indexer",
    "IndexerOptions",
    "DefaultIndexer",
]
