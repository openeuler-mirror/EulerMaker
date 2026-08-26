"""调度器资源模型

定义调度器的资源模型, 包括作业、执行机、资源容量等。

包含：
    - CoreInfo: CPU/GPU内核信息
        - cores: 核心总数
        - allocatable: 剩余核心数
        
    - NumaNode: NUMA 节点信息（物理机场景）
        - node_id: 节点id
        - memory: 内存总量(单位: MB)
        - allocatable_memory: 剩余内存(单位: MB)
        - cores: CPU列表

    - CapacityInfo: 磁盘/内存容量信息
        - capacity: 总容量
        - allocatable: 剩余容量

    - RunnerCapacity: 执行机资源容量
        - cpu: CPU核心信息
        - memory: 内存容量信息(单位: MB)
        - disk: 磁盘容量信息(单位: GB)
        - gpu: GPU核心信息
        - vram: GPU显存容量信息(单位: MB)
        - gpu_model: GPU模型信息
        - numa_topology: NUMA拓扑信息

    - ResourceRequest: 资源请求
        - requests: 资源需求
        - limits: 资源限制
        - nodeSelector: 节点选择器
        - tolerations: 污点容忍度
"""
from typing import Mapping, List, Optional, Tuple
from dataclasses import dataclass, field
from enum import Enum

from .utils import find_resource_metric_func

from .spec_models import (
    JobSpec,
    Toleration,
    ResourceQuantity
)

from .types import (
    ActionStatus,
    ActionErrorReason,
)

from .constants import (
    DEFAULT_RES_CPU,
    DEFAULT_RES_MEMORY,
    DEFAULT_RES_NOT_LIMIT
)


@dataclass
class NumaNode:
    """NUMA 节点信息（物理机场景）"""
    node_id:    int = 0  # 节点id
    memory:     int = 0  # 内存总量(单位：MB)
    allocatable_memory:   int = 0  # 剩余内存
    cores: List[int] = field(default_factory=list)  # CPU列表


@dataclass
class CoreInfo:
    """CPU/GPU内核信息"""
    cores:          float = 0.0  # 核心总数
    allocatable:    float = 0.0  # 剩余核心数


@dataclass
class CapacityInfo:
    """磁盘/内存容量信息"""
    capacity:       int = 0  # 内存总容量
    allocatable:    int = 0  # 剩余内存数


class CapacityField(Enum):
    ALLOCATABLE = 'allocatable'
    CAPACITY = 'capacity'


@dataclass
class RunnerCapacity:
    """Runner 当前资源容量

    由 ResourceCalcPlugin.calculate() 计算得出。
    不同 Runner 类型关注的字段不同：
      - CT: 关注 memory, cpu, disk
      - VM: 关注 memory, cpu, disk(从 Hypervisor 读取)
      - HW: 关注 memory, cpu, disk, gpu, numa_topology
    """

    # ─── 通用资源 ───────────────────────────────────────
    cpu:    CoreInfo = field(default_factory=CoreInfo)  # cpu核信息
    memory: CapacityInfo = field(default_factory=CapacityInfo)  # 内存信息
    disk:   CapacityInfo = field(default_factory=CapacityInfo)  # 磁盘信息

    # ─── GPU 资源（物理机场景） ──────────────────────────
    gpu:    CoreInfo = field(default_factory=CoreInfo)  # gpu核信息
    vram:   CapacityInfo = field(default_factory=CapacityInfo)  # gpu显存信息
    gpu_model: str = ""     # gpu model

    # ─── 存储资源 ──────────────────────────
    ephemeral_storage: CapacityInfo = field(default_factory=CapacityInfo)  # 临时存储信息

    # ─── NUMA 拓扑（物理机场景） ──────────────────────────
    numa_topology: List[NumaNode] = field(default_factory=list)

    def _satisfy(self, req: "JobRequirement", metrics: List[str], field_name: CapacityField) -> bool:
        """检查资源是否满足需求（allocatable 或 capacity）"""
        all_metrics = self._metrics()
        metric_names = [c[0] for c in all_metrics]

        for metric in metrics:
            if metric not in metric_names:
                return False

        for metric, alloc_field, capacity_field in all_metrics:
            if metric not in metrics:
                continue

            if not hasattr(req, metric) or not hasattr(self, metric):
                return False

            req_val = getattr(req, metric)
            if isinstance(req_val, bool) or not isinstance(req_val, (int, float)):
                continue

            field = capacity_field if field_name == CapacityField.CAPACITY else alloc_field
            if req_val > 0 and getattr(getattr(self, metric), field) < req_val:
                return False
        return True

    def allocatable_satisfy(self, req: "JobRequirement", metrics: List[str] = ['cpu', 'memory']) -> bool:
        """检查是否满足资源需求（可分配量）"""
        return self._satisfy(req, metrics, CapacityField.ALLOCATABLE)

    def capacity_satisfy(self, req: "JobRequirement",  metrics: List[str] = ['cpu', 'memory']) -> bool:
        """检查是否满足资源需求（总容量）"""
        return self._satisfy(req, metrics, CapacityField.CAPACITY)

    def score_metrics(self) -> List[Tuple[str, str, str]]:
        """返回 RunnerCapacity 中的所有可用于打分的字段名"""
        return self._metrics()

    def _metrics(self) -> List[Tuple[str, str, str]]:
        """返回 RunnerCapacity 中的所有可用于打分的字段名"""
        return [
            ("cpu",    "allocatable", "cores"),
            ("memory", "allocatable", "capacity"),
            ("disk",   "allocatable", "capacity"),
            ("gpu",    "allocatable", "cores"),
            ("vram",   "allocatable", "capacity")
        ]

    def subtract(self, req: "JobRequirement"):
        """从当前容量中减去另一个需求"""
        for metric, allocatable, _ in self._metrics():
            if hasattr(req, metric) and not isinstance(getattr(req, metric), bool) \
                    and isinstance(getattr(req, metric), (int, float)) \
                    and getattr(req, metric) > 0:
                current = getattr(getattr(self, metric), allocatable)
                new_value = current - getattr(req, metric)
                if new_value < 0:
                    raise ValueError(
                        f"Resource {metric} {allocatable} would become negative: "
                        f"{current} - {getattr(req, metric)} = {new_value}"
                    )
                setattr(getattr(self, metric), allocatable, new_value)

    def summary(self) -> dict:
        """生成容量摘要，用于日志"""
        return {
            "cpu": f"{self.cpu.allocatable:.1f}/{self.cpu.cores:.1f}",
            "memory": f"{self.memory.allocatable}/{self.memory.capacity}",
            "disk": f"{self.disk.allocatable}/{self.disk.capacity}",
            "gpu": f"{self.gpu.allocatable}/{self.gpu.cores}" if self.gpu.cores > 0 else "N/A",
        }

    def allocatable_print(self) -> dict:
        """生成可分配资源摘要，用于日志"""
        return {
            "cpu": f"{self.cpu.allocatable}",
            "memory": f"{self.memory.allocatable}",
            "disk": f"{self.disk.allocatable}",
            "gpu": f"{self.gpu.allocatable}",
            "vram": f"{self.vram.allocatable}",
        }

    def capacity_print(self) -> dict:
        """生成总资源容量摘要，用于日志"""
        return {
            "cpu": f"{self.cpu.cores}",
            "memory": f"{self.memory.capacity}",
            "disk": f"{self.disk.capacity}",
            "gpu": f"{self.gpu.cores}",
            "vram": f"{self.vram.capacity}",
        }


@dataclass
class JobRequirement:
    """Job 当前资源容量"""
    cpu:        int = field(default=DEFAULT_RES_CPU)
    memory:     int = field(default=DEFAULT_RES_MEMORY)
    disk:       int = field(default=DEFAULT_RES_NOT_LIMIT)
    gpu:        int = field(default=DEFAULT_RES_NOT_LIMIT)
    vram:       int = field(default=DEFAULT_RES_NOT_LIMIT)
    gpu_model:  str = ""     # gpu model
    ephemeral_storage: int = field(default=DEFAULT_RES_NOT_LIMIT)  # 临时存储信息

    # ResourceQuantity (camelCase) → JobRequirement (snake_case) 字段名映射
    _FIELD_NAME_MAP = {
        "gpu_model": "gpuModel",
        "ephemeral_storage": "ephemeralStorage",
    }

    @staticmethod
    def from_resource_quantity(res_quantity: Optional[ResourceQuantity]) -> Optional['JobRequirement']:
        """从ResourceQuantity创建JobRequirement"""
        requirement = JobRequirement()

        if res_quantity is None:
            return requirement

        for key, _ in JobRequirement.__dataclass_fields__.items():
            res_key = JobRequirement._FIELD_NAME_MAP.get(key, key)
            if hasattr(res_quantity, res_key):
                func = find_resource_metric_func(key)
                setattr(requirement, key, func(getattr(res_quantity, res_key)))

        return requirement


@dataclass
class ResourceRequest:
    """
    Job对Runner的资源需求, jobSpec字段中解析, 包含字段：
    - requests      → 资源需求
    - limits        → 资源限制
    - nodeSelector  → 节点选择器
    - tolerations   → 污点容忍度
    """
    # 资源需求
    requests: Optional[JobRequirement] = None
    limits:   Optional[JobRequirement] = None

    # 节点选择器
    node_selector: Mapping[str, str] = field(default_factory=dict)

    # 容忍度
    tolerations: List[Toleration] = field(default_factory=list)

    @staticmethod
    def from_job_spec(job_spec: JobSpec) -> 'ResourceRequest':
        """
        从JobSpec创建ResourceRequest
        """
        resources = job_spec.resources if job_spec.resources else None

        # requests 或 limits 没限制时，使用默认资源
        requests = JobRequirement.from_resource_quantity(resources.requests if resources else None)
        limits = JobRequirement.from_resource_quantity(resources.limits if resources else None)

        return ResourceRequest(
            requests=requests,
            limits=limits,
            node_selector=job_spec.nodeSelector if job_spec.nodeSelector else {},
            tolerations=job_spec.tolerations if job_spec.tolerations else [],
        )


@dataclass
class ActionResult:
    """操作结果"""
    status: ActionStatus       # success, failed
    reason: Optional[ActionErrorReason]  # 失败原因；成功时为 None
    msg: str = ""              # 执行信息
