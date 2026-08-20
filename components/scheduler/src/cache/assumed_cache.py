"""Runner 资源预占缓存

Scheduler 在选出 Runner 后、发送 Bind 请求前，通过 assumed cache 原子预占 Runner 资源，
防止 apiserver 更新成功到 Job watch 事件进入本地 Indexer 之间的时间窗口内造成资源超卖。

设计文档: docs/zh/design/scheduler.md §5.2.5
"""

import threading
import time
from dataclasses import dataclass, field
from typing import Callable, Dict, List, Optional, Set

from log import debug, warning

from common import JobRequirement


@dataclass
class AssumedJob:
    """预占 Job 记录"""

    job_key: str
    """Job 唯一键，格式: {project}/{jobName}"""

    job_uid: str
    """Job UID，防止同名 Job 删除重建后误匹配"""

    runner_name: str
    """预选 Runner 名称"""

    requests: JobRequirement
    """调度时使用的资源请求快照"""

    resource_version: str
    """发起 Bind 前观察到的 Job resourceVersion"""

    assumed_at: float = field(default_factory=time.time)
    """预占时间（Unix 时间戳）"""


class RunnerAssumedCache:
    """Runner 资源预占缓存

    线程安全：所有公共方法内部使用 threading.RLock（可重入锁）保护。
    对同一个 Runner 的"读取 → 判断 → 写入"必须在同一临界区内完成，
    调用方应使用 assume() 的 check_fn 回调在临界区内完成原子操作。
    """

    def __init__(self, expire_timeout: float = 30.0):
        """
        Args:
            expire_timeout: 预占确认超时时间（秒），超时后需从 apiserver GET 确认绑定状态
        """
        self._lock = threading.RLock()
        self._expire_timeout = expire_timeout

        # 主存储: job_key → AssumedJob
        self._by_job_key: Dict[str, AssumedJob] = {}

        # 索引: runner_name → set of job_key
        self._by_runner: Dict[str, Set[str]] = {}

    # --- 公共 API ---

    def assume(
        self,
        job_key: str,
        job_uid: str,
        runner_name: str,
        requests: JobRequirement,
        resource_version: str,
        check_fn: Optional[Callable[[], bool]] = None,
    ) -> bool:
        """原子预占 Runner 资源

        在临界区内执行 check_fn（如果提供）并写入预占记录。
        如果 check_fn 返回 False，不写入预占并返回 False。

        Args:
            job_key: Job 唯一键 {project}/{jobName}
            job_uid: Job UID
            runner_name: 预选 Runner 名称
            requests: 资源请求快照
            resource_version: Job resourceVersion
            check_fn: 可选的资源可用性检查回调，在临界区内执行

        Returns:
            True 表示预占成功，False 表示资源不足或已存在同 key 记录
        """
        with self._lock:
            if job_key in self._by_job_key:
                return False

            if check_fn is not None and not check_fn():
                return False

            record = AssumedJob(
                job_key=job_key,
                job_uid=job_uid,
                runner_name=runner_name,
                requests=requests,
                resource_version=resource_version,
            )
            self._by_job_key[job_key] = record

            if runner_name not in self._by_runner:
                self._by_runner[runner_name] = set()
            self._by_runner[runner_name].add(job_key)

            debug(
                f"assumed cache: assumed job {job_key} on runner {runner_name}, "
                f"requests={requests}"
            )
            return True

    def forget(self, job_key: str) -> Optional[AssumedJob]:
        """删除预占记录

        用于以下场景：
        - Bind API 返回失败或 resourceVersion 冲突
        - 调用方已确认可以安全删除

        Args:
            job_key: Job 唯一键

        Returns:
            被删除的 AssumedJob 记录，如果不存在则返回 None
        """
        with self._lock:
            record = self._by_job_key.pop(job_key, None)
            if record is None:
                return None

            runner_jobs = self._by_runner.get(record.runner_name)
            if runner_jobs is not None:
                runner_jobs.discard(job_key)
                if not runner_jobs:
                    del self._by_runner[record.runner_name]

            debug(f"assumed cache: forgot job {job_key} from runner {record.runner_name}")
            return record

    def forget_if_match(
        self,
        job_key: str,
        job_uid: str,
        runner_name: str,
    ) -> Optional[AssumedJob]:
        """按条件删除预占记录

        仅当预占记录与给定 UID 和 Runner 名称完全匹配时才删除，
        防止同名 Job 删除重建后误释放，或 Job 被绑定到不同 Runner 时误释放。

        适用场景：
        - Job watch 观察到同一 UID 的 Job 已处于 Running 且 status.runner 与预占一致
        - Job 被删除，状态/UID/Runner 与预占不一致时安全释放

        Args:
            job_key: Job 唯一键
            job_uid: Job UID（用于匹配验证）
            runner_name: Runner 名称（用于匹配验证）

        Returns:
            被删除的 AssumedJob 记录，如果不存在或不匹配则返回 None
        """
        with self._lock:
            record = self._by_job_key.get(job_key)
            if record is None:
                return None

            if record.job_uid != job_uid:
                debug(
                    f"assumed cache: forget_if_match uid mismatch for {job_key}, "
                    f"expected={record.job_uid}, got={job_uid}"
                )
                return None

            if record.runner_name != runner_name:
                debug(
                    f"assumed cache: forget_if_match runner mismatch for {job_key}, "
                    f"expected={record.runner_name}, got={runner_name}"
                )
                return None

            del self._by_job_key[job_key]
            runner_jobs = self._by_runner.get(record.runner_name)
            if runner_jobs is not None:
                runner_jobs.discard(job_key)
                if not runner_jobs:
                    del self._by_runner[record.runner_name]

            debug(
                f"assumed cache: forget_if_match released job {job_key} "
                f"from runner {record.runner_name}"
            )
            return record

    def get_by_job_key(self, job_key: str) -> Optional[AssumedJob]:
        """按 Job Key 查询预占记录"""
        with self._lock:
            return self._by_job_key.get(job_key)

    def get_by_runner(self, runner_name: str) -> List[AssumedJob]:
        """获取指定 Runner 上的所有预占 Job"""
        with self._lock:
            job_keys = self._by_runner.get(runner_name, set())
            return [self._by_job_key[k] for k in job_keys if k in self._by_job_key]

    def get_assumed_requests_for_runner(self, runner_name: str) -> Dict[str, int]:
        """汇总指定 Runner 上所有预占 Job 的资源请求总量

        Returns:
            聚合后的资源字典，如 {"cpu": 8, "memory": 16384}
        """
        with self._lock:
            job_keys = self._by_runner.get(runner_name, set())
            total: Dict[str, int] = {}
            for k in job_keys:
                record = self._by_job_key.get(k)
                if record is None:
                    continue
                for resource_name in JobRequirement.__dataclass_fields__:
                    val = getattr(record.requests, resource_name, 0)
                    if val > 0:
                        total[resource_name] = total.get(resource_name, 0) + val
            return total

    def get_assumed_requirements_for_runner(self, runner_name: str) -> List[JobRequirement]:
        """获取指定 Runner 上所有预占 Job 的 JobRequirement 列表"""
        with self._lock:
            job_keys = self._by_runner.get(runner_name, set())
            return [self._by_job_key[k].requests for k in job_keys if k in self._by_job_key]

    def has_assumed(self, job_key: str) -> bool:
        """检查指定 Job Key 是否存在预占记录"""
        with self._lock:
            return job_key in self._by_job_key

    def runner_assumed_count(self, runner_name: str) -> int:
        """获取指定 Runner 上的预占 Job 数量"""
        with self._lock:
            return len(self._by_runner.get(runner_name, set()))

    def total_assumed(self) -> int:
        """获取预占记录总数"""
        with self._lock:
            return len(self._by_job_key)

    # --- 过期清理 ---

    def get_expired(self) -> List[AssumedJob]:
        """获取所有已过期的预占记录

        返回的过期记录仍需调用方从 apiserver GET 确认绑定状态后才能 forget。
        设计文档要求：超时后不能直接释放，必须先 GET 确认绑定未生效。

        Returns:
            过期的 AssumedJob 列表
        """
        now = time.time()
        with self._lock:
            return [
                r for r in self._by_job_key.values()
                if now - r.assumed_at > self._expire_timeout
            ]

    def cleanup_expired(
        self,
        confirm_fn: Callable[[AssumedJob], bool],
    ) -> List[str]:
        """清理过期预占记录

        对每条过期记录调用 confirm_fn 确认绑定状态。
        只有 confirm_fn 返回 True（确认绑定未生效）时才删除记录。

        Args:
            confirm_fn: 确认回调，参数为 AssumedJob，返回 True 表示可以安全删除

        Returns:
            被清理的 job_key 列表
        """
        expired = self.get_expired()
        cleaned: List[str] = []
        for record in expired:
            try:
                if confirm_fn(record):
                    self.forget(record.job_key)
                    cleaned.append(record.job_key)
                else:
                    warning(
                        f"assumed cache: expired job {record.job_key} still bound, "
                        f"keeping record"
                    )
            except Exception as e:
                warning(
                    f"assumed cache: failed to confirm expired job {record.job_key}: {e}"
                )
        return cleaned

    # --- 内部方法 ---

    def _get_all_runner_names(self) -> Set[str]:
        """获取所有有预占记录的 Runner 名称"""
        with self._lock:
            return set(self._by_runner.keys())

    def __len__(self) -> int:
        return self.total_assumed()

    def __contains__(self, job_key: str) -> bool:
        return self.has_assumed(job_key)
