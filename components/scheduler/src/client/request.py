from urllib3 import PoolManager
from urllib.parse import urlencode, quote
from typing import Any, Dict, Optional, Type
from log import error

from .response import Response
from .decoder import Decoder
from .types import RequestMethod

_RUNNERS_RESOURCE_TYPE = 'runners'


class Request:

    def __init__(
        self,
        host: str,
        pool: PoolManager,
        headers: Optional[Dict[str, str]] = None,
        params: Optional[Dict[str, Any]] = None,
        timeout: int = 10,
        decoder: Optional[Type[Decoder]] = None
    ):
        self._pool = pool
        self._host = host
        self._decoder = decoder

        self._resource_type = None
        self._namespace = None
        self._name = None
        self._params = dict(params or {})
        self._headers = dict(headers or {})
        self._timeout = timeout
        self._body = None
        self._status: bool = False

    def resource(self, resource_type: str) -> 'Request':
        if not resource_type:
            raise ValueError(f"resource_type {resource_type} is not valid")
        self._resource_type = resource_type
        return self

    def namespace(self, namespace: str) -> 'Request':
        self._namespace = namespace
        return self

    def name(self, name: str) -> 'Request':
        if not name:
            raise ValueError("name is empty")
        self._name = name
        return self

    def url_params(self, params: Dict[str, Any]) -> 'Request':
        if params is not None:
            self._params.update(params)
        return self

    def body(self, body: Any) -> 'Request':
        if body is not None:
            self._body = body
        return self

    def header(self, headers: Dict[str, str]) -> 'Request':
        if headers is not None:
            self._headers = self._merge_headers(self._headers, headers)
        return self

    def status(self) -> 'Request':
        self._status = True
        return self

    def do_post(self) -> Response:
        return self._do_request(method=RequestMethod.POST,
                                url=self._build_url(),
                                params=self._params,
                                body=self._body,
                                headers=self._headers)

    def do_get(self) -> Response:
        return self._do_request(method=RequestMethod.GET,
                                url=self._build_url(),
                                params=self._params,
                                body=self._body,
                                headers=self._headers)

    def do_patch(self) -> Response:
        # 拷贝后再加 PATCH 专属的 Content-Type, 避免污染 self._headers 影响后续请求
        headers = dict(self._headers)
        if not self._has_header(headers, 'Content-Type'):
            headers['Content-Type'] = 'application/merge-patch+json'

        return self._do_request(method=RequestMethod.PATCH,
                                url=self._build_url(),
                                params=self._params,
                                body=self._body,
                                headers=headers)

    def do_put(self) -> Response:
        return self._do_request(method=RequestMethod.PUT,
                                url=self._build_url(),
                                params=self._params,
                                body=self._body,
                                headers=self._headers)

    def do_delete(self) -> Response:
        return self._do_request(method=RequestMethod.DELETE,
                                url=self._build_url(),
                                params=self._params,
                                body=self._body,
                                headers=self._headers)

    def list(self) -> Response:
        return self._do_request(method=RequestMethod.GET,
                                url=self._build_url(),
                                params=self._params,
                                body=self._body,
                                headers=self._headers)

    def watch(self) -> Response:
        params = dict(self._params)
        params['watch'] = 'true'
        return self._do_request(method=RequestMethod.GET,
                                url=self._build_url(),
                                params=params,
                                body=self._body,
                                headers=self._headers,
                                _preload_content=False)

    def _build_url(self) -> str:
        if not self._resource_type:
            raise ValueError("resource_type is required, call .resource() first")

        base_url = [self._host]

        if self._namespace and self._resource_type != _RUNNERS_RESOURCE_TYPE:
            base_url.extend(['projects', quote(self._namespace, safe='')])

        base_url.append(self._resource_type.lower())

        if self._name:
            base_url.append(quote(self._name, safe=''))

        if self._status:
            base_url.append('status')

        return "/".join(base_url)

    def _do_request(self, method: RequestMethod,
                    url: str,
                    body: Any = None,
                    headers: Optional[Dict[str, str]] = None,
                    params: Optional[Dict[str, Any]] = None,
                    _preload_content: bool = True,
                    timeout: Optional[int] = None) -> Response:
        headers = dict(self._headers if headers is None else headers)
        params = dict(self._params if params is None else params)
        body = self._body if body is None else body
        timeout = timeout if timeout is not None else self._timeout

        if body is not None and not self._has_header(headers, 'Content-Type'):
            headers['Content-Type'] = 'application/json'

        if 'watch' not in params:
            params['watch'] = 'false'

        if params.get('watch') == 'true':
            timeout = None

        if params:
            url += f"?{urlencode(params)}"

        try:
            resp = self._pool.request(
                method.value,
                url,
                headers=headers,
                body=body,
                preload_content=_preload_content,
                timeout=timeout
            )

            return Response(resp) if not self._decoder else self._decoder(Response(resp))
        except Exception as e:
            error(f"Request failed: {method.value} {url}, error: {e}")
            raise

    @staticmethod
    def _has_header(headers: Dict[str, str], name: str) -> bool:
        return any(key.lower() == name.lower() for key in headers)

    @staticmethod
    def _merge_headers(target: Dict[str, str], source: Dict[str, str]) -> Dict[str, str]:
        merged = {
            key: value
            for key, value in target.items()
            if all(key.lower() != source_key.lower() for source_key in source)
        }
        merged.update(source)
        return merged
