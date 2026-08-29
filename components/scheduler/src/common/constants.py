"""常量定义

Runner标签常量包含:
    - LABEL_RUNNER_TYPE: 运行器类型标签, 如："ebs.io/runner-type": "ct"
    - LABEL_RUNNER_ARCH: 架构标签， 如："ebs.io/arch": "x86_64"
    - LABEL_RUNNER_ZONE: 区域标签， 如："ebs.io/zone": "east"

索引常量包含:
    - INDEX_KIND: 索引类型常量, 用于标识索引的类型。
    - INDEX_METADATA_NAME: 索引键常量, 用于标识索引键的名称属性。
    - INDEX_METADATA_NAMESPACE: 索引键常量, 用于标识索引键的命名空间属性。
    - INDEX_METADATA_LABELS: 索引键常量, 用于标识索引键的标签属性。
    - INDEX_BOUNDED_RUNNER: 索引键常量, 用于标识索引键的绑定 Runner 属性。
    - INDEX_STATUS_PHASE: 索引键常量, 用于标识索引键的阶段属性。
"""
# ================================ Runner标签常量 ==========================
# 运行器类型标签, 如："ebs.io/runner-type": "ct"
LABEL_RUNNER_TYPE = "ebs.io/runner-type"
# 架构标签， 如："ebs.io/runner-arch": "x86_64"
LABEL_RUNNER_ARCH = "ebs.io/runner-arch"
# 区域标签， 如："ebs.io/zone": "east"
LABEL_RUNNER_ZONE = "ebs.io/zone"

# ================================ 索引器常量 ==========================
# 索引器资源类型
INDEX_KIND = "kind"

# 索引器资源名称
INDEX_METADATA_NAME = "metadata.name"

# 索引器标签
INDEX_METADATA_LABELS = "metadata.labels"

# 索引器命名空间
INDEX_METADATA_NAMESPACE = "metadata.namespace"

# 索引器绑定 Runner
INDEX_BOUNDED_RUNNER = "status.runner"

# 索引器状态阶段
INDEX_STATUS_PHASE = "status.phase"

# ================================ Job 默认值 ==========================
# CPU 默认资源: 2 核(单位: 毫核, 1000 毫核 = 1 核)
DEFAULT_RES_CPU = 2000
DEFAULT_RES_CPU_STR = "2000m"

# MEMORY 默认资源
DEFAULT_RES_MEMORY = 8589934592  # 默认：8GiB, 单位：Byte
DEFAULT_RES_MEMORY_STR = "8Gi"

# -1 表示无限制
DEFAULT_RES_NOT_LIMIT = -1
DEFAULT_RES_NOT_LIMIT_STR = "-1"

# 优先级默认值
DEFAULT_PRIORITY = 0

# 默认运行超时时间, 默认3小时
DEFAULT_TIMEOUT_SECONDS = 10800

# ================================ API SERVER ==========================
# 资源版本
API_VERSION = "ebs/v1"


__all__ = [
    # 标签
    "LABEL_RUNNER_TYPE",
    "LABEL_RUNNER_ARCH",
    "LABEL_RUNNER_ZONE",

    # 索引
    "INDEX_KIND",
    "INDEX_METADATA_NAME",
    "INDEX_METADATA_LABELS",
    "INDEX_METADATA_NAMESPACE",
    "INDEX_BOUNDED_RUNNER",
    "INDEX_STATUS_PHASE",

    # 资源
    "DEFAULT_RES_CPU",
    "DEFAULT_RES_CPU_STR",
    "DEFAULT_RES_MEMORY",
    "DEFAULT_RES_MEMORY_STR",
    "DEFAULT_RES_NOT_LIMIT",
    "DEFAULT_RES_NOT_LIMIT_STR",
    "DEFAULT_PRIORITY",
    "DEFAULT_TIMEOUT_SECONDS",

    "API_VERSION",
]
