import ssl
from urllib3 import (
    PoolManager,
    Retry
)
from typing import Any, Dict, Optional, Type
from .decoder import Decoder
from .request import Request

# api server 服务接口版本
_API_BASE_URL = "apis/ebs/v1"


class EBSClient:
    def __init__(
        self,
        host: str,
        version: str = None,
        cert_file: str = None,
        key_file: str = None,
        pool_size: int = 4,
        max_size: int = 4,
        ssl_verify: bool = False,
        timeout: int = 30,
    ):
        if not host:
            raise ValueError("host must be a non-empty string")

        host = host.strip().rstrip('/')
        if not host.startswith(('http://', 'https://')):
            raise ValueError("host must start with http:// or https://")

        # 配置重试策略: 5次连接重试, 对500, 502, 503, 504状态码重试
        retry_strategy = Retry(connect=5,
                               backoff_factor=0.3,
                               backoff_max=30,
                               status_forcelist=[500, 502, 503, 504])

        cert_reqs = ssl.CERT_REQUIRED if ssl_verify else ssl.CERT_NONE
        self.pool = PoolManager(num_pools=pool_size,
                                maxsize=max_size,
                                cert_file=cert_file,
                                key_file=key_file,
                                cert_reqs=cert_reqs,
                                retries=retry_strategy)

        if version:
            host = f"{host}/apis/ebs/{version}"
        else:
            host = f"{host}/{_API_BASE_URL}"

        self.host = host
        self.timeout = timeout

    def create_request(self,
                       headers: Optional[Dict[str, str]] = None,
                       params: Optional[Dict[str, Any]] = None,
                       timeout: Optional[int] = None,
                       decoder: Optional[Type[Decoder]] = None):

        timeout = timeout if timeout is not None else self.timeout
        return Request(self.host, self.pool, headers, params, timeout, decoder=decoder)
