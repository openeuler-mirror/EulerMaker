"""资源spec数据模型

定义资源的spec数据模型, 包括资源的元数据、头信息、规格和状态。

包含：
    - Metadata: 资源元数据
        - uid: 系统生成唯一ID
        - name: 资源名称
        - namespace: 命名空间
        - resourceVersion: 乐观锁版本号
        - creationTimestamp: 创建时间
        - deletionTimestamp: 删除标记时间
        - finalizers: 删除前清除操作
        - labels: 标签
        - annotations: 注释

    - SpecHeader: 资源头信息
        - apiVersion: 资源版本
        - kind: 资源类型
        - metadata: 资源元数据

    - JobSpec: 作业规格
        - priority: 优先级
        - runtimeSpec: 运行时专属配置
        - timeoutSeconds: 运行时间(秒)
        - resources: 资源需求
        - nodeSelector: 节点选择器
        - tolerations: 污点容忍度
        - payload: 作业负载

    - JobStatus: 作业状态
        - phase: 执行阶段： Pending、Running、Completed、Failed、Aborted
        - stage: Pending、Running、PostRun、Failed
        - runner: 执行机名称
        - startTime: 开始时间
        - endTime: 结束时间
        - resultRoot: 结果存储根目录
        - message: 执行信息
        - restartCount: 重试次数
    
    - Job: 作业资源
        - spec: 作业规格
        - status: 作业状态

    - JobList: 作业资源列表
        - items: 作业资源列表

    - RunnerTaint: 污点
        - key: 污点键
        - value: 污点值
        - effect: 污点效果, 如: NoSchedule, PreferNoSchedule, NoExecute

    - RunnerSpec: 执行机规格
        - type: 执行机类型,如: CT, VM, HW
        - arch: cpu架构如: x86_64
        - hostname: 宿主机名称
        - unschedulable: 是否可调度, 默认False
        - taints: 污点列表

    - RunnerAddress: 执行机地址
        - type: 执行机地址类型, 如: IP, Hostname
        - address: 执行机IP地址或主机名 

    - RunnerInfo: 执行机信息
        - os: 操作系统, 如: Linux, Windows
        - kernelVersion: 内核版本
        - arch: cpu架构如: x86_64
        - runtimeVersion: 运行时版本
        - agentVersion: 执行机代理版本

    - RunnerStatus: 执行机状态
        - phase: 执行阶段： Registering、Booting、Running、Idle、Offline
        - conditions: 执行状态列表
        - capacity:  资源总容量
        - allocatable: 可调度容量
        - addresses: 执行机地址列表
        - info: 执行机信息
        - heartbeat: 执行机心跳时间

    - Runner: 执行机资源
        - spec: 执行机规格
        - status: 执行机状态

    - RunnerList: 执行机资源列表
        - items: 执行机资源列表

"""
from pydantic import Field, BaseModel, ConfigDict
from typing import Any, List, Mapping, Optional

from .types import JobPhase, RunnerPhase

from .constants import (
    DEFAULT_RES_CPU_STR,
    DEFAULT_RES_MEMORY_STR,
    DEFAULT_RES_NOT_LIMIT_STR,
    DEFAULT_PRIORITY,
    DEFAULT_TIMEOUT_SECONDS
)


class ResourceQuantity(BaseModel):
    '''
    资源数量模型
    定义资源的数量, 包括cpu、memory、disk、gpu数量。
    '''
    # 支持字段名称和别名初始化配置
    model_config = ConfigDict(populate_by_name=True)

    cpu:      Optional[str] = Field(default=DEFAULT_RES_CPU_STR, description="CPU核心数, 默认2核")
    memory:   Optional[str] = Field(default=DEFAULT_RES_MEMORY_STR, description="内存需求, 默认8Gi")
    disk:     Optional[str] = Field(default=DEFAULT_RES_NOT_LIMIT_STR, description="磁盘需求, 默认-1表示不关注磁盘需求")
    gpu:      Optional[str] = Field(default=DEFAULT_RES_NOT_LIMIT_STR, description="GPU数量, 默认-1表示不关注gpu数量")
    vram:     Optional[str] = Field(default=DEFAULT_RES_NOT_LIMIT_STR, description="GPU显存需求, 默认-1表示不关注显存需求")
    gpuModel: Optional[str] = Field(default="", description="GPU模型, 为空表示不关注gpu模型")
    ephemeralStorage: Optional[str] = Field(default=DEFAULT_RES_NOT_LIMIT_STR,
                                            alias="ephemeral-storage", description="临时存储, 默认-1表示不关注")


class Metadata(BaseModel):
    uid:                Optional[str] = None  # 系统生成唯一ID
    name:               Optional[str] = None  # 资源名称
    namespace:          Optional[str] = None  # 命名空间
    resourceVersion:    Optional[str] = None  # 乐观锁版本号
    creationTimestamp:  Optional[str] = None  # 创建时间
    deletionTimestamp:  Optional[str] = None  # 删除标记时间
    finalizers:     List[str] = Field(default_factory=list)  # 删除前清除操作
    labels:         Mapping[str, str] = Field(default_factory=dict)  # 标签
    annotations:    Mapping[str, str] = Field(default_factory=dict)  # 注释


class SpecHeader(BaseModel):
    # 版本：ebs/v1
    apiVersion: str = Field(...)
    # 资源类型： Project、Snapshot、Build、Job, Runner
    kind:       str = Field(...)
    # 资源元数据
    metadata:   Metadata = Field(default_factory=Metadata)


class Toleration(BaseModel):
    # 污点键
    key:        Optional[str] = Field(default=None)
    # Equal, Exists, Gt, Lt
    operator:   Optional[str] = Field(default=None)
    # 污点值
    value:      Optional[str] = Field(default=None)
    # NoSchedule, PreferNoSchedule, NoExecute
    effect:     Optional[str] = Field(default=None)


class ResourceRequirements(BaseModel):
    # 资源需求
    requests: Optional[ResourceQuantity] = None
    # 资源限制
    limits:    Optional[ResourceQuantity] = None


class JobSpec(BaseModel):
    # 优先级
    priority:    int = Field(default=DEFAULT_PRIORITY)

    # 执行运行时类型，如 ct/vm/hw，默认 ct
    runtime:     Optional[str] = Field(default="ct")

    # 运行时专属设置
    runtimeSpec: Optional[Any] = Field(default=None)

    # 运行超时时间(秒)，默认3小时
    timeoutSeconds: int = Field(default=DEFAULT_TIMEOUT_SECONDS)

    # 资源需求和限制
    resources: ResourceRequirements = Field(default_factory=ResourceRequirements)

    # 节点选择器
    nodeSelector: Mapping[str, str] = Field(default_factory=dict)

    # 容忍度
    tolerations: List[Toleration] = Field(default_factory=list)

    # Job 载荷，用于传递作业相关数据
    payload: Optional[str] = Field(default=None)


class JobStatus(BaseModel):
    # 执行阶段： Pending、Running、Completed、Failed、Aborted
    phase:      JobPhase = Field(...)
    # Pending、Running、PostRun、Failed
    stage:      Optional[str] = None
    # 执行机名称
    runner:     Optional[str] = None
    # 开始时间
    startTime:  Optional[str] = None
    # 结束时间
    endTime:    Optional[str] = None
    # 结果存储根目录
    resultRoot: Optional[str] = None
    # 状态信息
    message:    Optional[str] = None
    # 重试次数
    restartCount: int = Field(default=0)


class Job(SpecHeader):
    spec: Optional[JobSpec] = None
    status: Optional[JobStatus] = None


class JobList(SpecHeader):
    items: List[Job] = Field(default_factory=list)


class RunnerTaint(BaseModel):
    key:    Optional[str] = Field(default=None)  # 污点键
    value:  Optional[str] = Field(default=None)  # 污点值
    # NoSchedule, PreferNoSchedule, NoExecute
    effect: Optional[str] = Field(default=None)


class RunnerSpec(BaseModel):
    type:       Optional[str] = None  # 执行机类型：CT、VM、HW
    arch:       str = Field(...)  # CPU架构：x86_64、aarch64
    hostname:   Optional[str] = None  # 宿主机名称
    unschedulable: bool = Field(default=False)  # 是否禁止调度新Job

    # 反亲和污点
    taints: List[RunnerTaint] = Field(default_factory=list)


class RunnerAddress(BaseModel):
    # 地址类型：Hostname、InternalIP、ExternalIP
    type:       Optional[str] = None
    address:    Optional[str] = None  # 地址


class RunnerInfo(BaseModel):
    os:             Optional[str] = None  # 操作系统
    kernelVersion:  Optional[str] = None  # 内核版本
    arch:           Optional[str] = None  # CPU架构
    runtimeVersion: Optional[str] = None  # 执行机运行时
    agentVersion:   Optional[str] = None  # Runner agent版本


class RunnerStatus(BaseModel):
    """
    运行器状态
    """
    # 执行机状态：Registering、Booting、Running、Idle、Offline
    phase:          RunnerPhase = Field(...)
    conditions:     List[Any] = Field(default_factory=list)
    capacity:       Mapping[str, str] = Field(default_factory=dict)  # 资源总容量
    allocatable:    Mapping[str, str] = Field(default_factory=dict)  # 可调度容量
    addresses: List[RunnerAddress] = Field(default_factory=list)  # runner地址列表
    info:  RunnerInfo = Field(default_factory=RunnerInfo)  # 执行机系统与agent信息
    heartbeat:      Optional[Any] = None  # 最后心跳时间


class Runner(SpecHeader):
    spec: RunnerSpec = Field(...)
    status: Optional[RunnerStatus] = None


class RunnerList(SpecHeader):
    items: List[Runner] = Field(default_factory=list)
