"""枚举类型模块

这是一个枚举类型模块, 用于表示事件类型, 任务状态, 资源类型, 操作类型, 操作状态, 操作错误原因

包含以下枚举类型:

- EventType: 事件类型
- JobStatus: 任务状态
- ResourceType: 资源类型
- ActionType: 操作类型
- ActionStatus: 操作状态
- ActionErrorReason: 操作错误原因

"""
from enum import Enum


class EventType(str, Enum):
    """
    事件类型
    """
    ADDED = "ADDED"
    MODIFIED = "MODIFIED"
    DELETED = "DELETED"


class JobPhase(str, Enum):
    """
    任务状态
    """
    PENDING = "Pending"
    RUNNING = "Running"
    COMPLETED = "Completed"
    FAILED = "Failed"
    ABORTED = "Aborted"


class RunnerPhase(str, Enum):
    """
    运行器阶段
    """
    REGISTERING = "Registering"
    BOOTING = "Booting"
    IDLE = "Idle"
    RUNNING = "Running"
    OFFLINE = "Offline"


class ResourceType(str, Enum):
    """
    资源类型（用于索引存储键）
    """
    JOB = "Jobs"
    RUNNER = "Runners"


class KindType(str, Enum):
    """
    资源 Kind 类型（用于资源对象标识）
    """
    JOB = "Job"
    RUNNER = "Runner"

    @staticmethod
    def from_resource_type(resource_type: ResourceType) -> 'KindType':
        '''
        根据资源类型返回对应的KindType
        '''
        if resource_type not in _KIND_TYPE_RESOURCE_MAP:
            raise ValueError(
                f"Unknown resource type: {resource_type}. "
                f"Supported: {list(_KIND_TYPE_RESOURCE_MAP)}"
            )
        return _KIND_TYPE_RESOURCE_MAP[resource_type]


_KIND_TYPE_RESOURCE_MAP = {
    ResourceType.JOB: KindType.JOB,
    ResourceType.RUNNER: KindType.RUNNER,
}


class ActionType(str, Enum):
    '''
    操作类型
    '''
    INITIAL = "initial"              # 初始化上下文
    CALC_CAPACITY = "calc_capacity"  # 计算 Runner 的当前资源容量和使用量， 默认插件
    FILTER = "filter"                # 过滤不满足条件的 Runner
    SCORE = "score"                  # 对候选 Runner 打分
    BIND = "bind"                    # 绑定 Runner 到 Job
    BROADCAST = "broadcast"          # 更新 Job 到 api_server


class SchedulerStepType(str, Enum):
    '''
    调度操作步骤类型
    '''
    PRE_SCHEDULER = "pre_scheduler"   # 预调度操作
    SCHEDULER = "scheduler"       # 调度操作
    POST_SCHEDULER = "post_scheduler"  # 后调度操作


class ActionStatus(str, Enum):
    """
    操作状态
    """
    SUCCESS = "SUCCESS"
    FAILED = "FAILED"
    SKIP = "SKIP"


class ActionErrorReason(Enum):
    # 当前资源不足
    UNSCHEDULABLE = 'the currently available resources do not meet the requested demand'

    # 需求超出总资源容量
    OUT_OF_CAPACITY = 'the resource requirements of the request exceed the total capacity of the resource'

    # 无runner资源
    UNABLE_FOUND_RUNNER = 'unable to find any runner, no one is available'

    # 需求资源未配置
    UNABLE_FOUND_RESOURCE_REQUEST = 'unable to find any resource request, please check the job spec'

    # 调度预处理失败
    PRE_SCHEDULER_FAILED = 'pre_scheduler process failed, detail reason look at the log'

    # prev action failed, skip current action
    PREV_ACTION_FAILED = 'prev action failed, skip current action'

    # filter plugin execute failed
    FILTER_PLUGIN_FAILED = 'filter plugin execute failed, detail reason look at the log'

    # score plugin execute failed
    SCORE_PLUGIN_FAILED = 'score plugin execute failed, detail reason look at the log'

    # bind plugin execute failed
    BIND_PLUGIN_FAILED = 'bind plugin execute failed, detail reason look at the log'

    # can not found any bind plugin available
    BIND_PLUGIN_NOT_SET = 'can not found any bind plugin available, please check the scheduler config'

    # over backoff limit
    ABORTED = 'job aborted, over backoff limit'

    RUNNER_TYPE_NOT_MATCH = 'runner type not match, please check the job spec'

    RUNNER_NOT_EXISTS = 'runner not exists, please check the runner spec'

    ALREADY_RUNNING = 'job already running on runner, skip'

    # unknown error
    UNKNOWN = 'unknown error'

    # scheduler internal error (unexpected exception in action handlers)
    INTERNAL_ERROR = 'scheduler internal error, detail reason look at the log'


class RunnerArch(str, Enum):
    '''
    运行器架构
    '''
    X86_64 = 'x86_64'  # 64位x86架构
    AARCH64 = 'aarch64'  # 64位ARM架构


class RunnerType(str, Enum):
    '''
    运行器类型
    '''
    CT = 'ct'  # container runner
    VM = 'vm'  # virtual machine runner
    HW = 'hw'  # hardware runner


RUNNER_TYPE_SPECS = {
    RunnerType.CT: {
        'name': 'container runner',
        'description': '运行在docker容器中的任务',
        "resource_source": "宿主机 cgroup/psutil",
        "scheduling_factors": ["MEMORY", "CPU", "DISK", "IMAGE_CACHE", "CACHE"]
    },
    RunnerType.VM: {
        'name': 'virtual machine runner',
        'description': '运行在虚拟机中的任务',
        "resource_source": "Hypervisor API (libvirt/proxmox)",
        "scheduling_factors": ["MEMORY", "CPU", "DISK", "VM_TEMPLATE_CACHE", "FRAGMENTATION_RATE"],
    },
    RunnerType.HW: {
        'name': 'hardware runner',
        'description': '运行在物理机中的任务',
        "resource_source": "宿主机 cgroup/psutil",
        "scheduling_factors": ["MEMORY", "CPU", "GPU", "NUMA_AFFINITY", "POWER/TEMPERATURE"]
    }
}
