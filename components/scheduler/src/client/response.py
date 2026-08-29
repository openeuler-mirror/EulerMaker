import json
from urllib3 import HTTPResponse
from typing import Any, Iterator, Optional


class ResponseError(Exception):
    """HTTP 响应状态码异常。"""

    def __init__(self, status_code: int, reason: Optional[str] = None):
        self.status_code = status_code
        self.reason = reason
        message = f"Request failed with status code {status_code}"
        if reason:
            message = f"{message}: {reason}"
        super().__init__(message)


class Response:
    def __init__(self, resp: HTTPResponse):
        self.res = resp
        self.status_code = self.res.status

    def __enter__(self) -> 'Response':
        return self

    def __exit__(self, exc_type, exc_val, exc_tb) -> bool:
        self.close()
        return False

    def is_ok(self) -> bool:
        return 200 <= self.status_code < 300

    def get_headers(self) -> dict:
        return dict(self.res.headers)

    def get_header(self, name: str, default: Optional[str] = None) -> Optional[str]:
        return self.res.headers.get(name, default)

    def close(self) -> None:
        length_remaining = getattr(self.res, 'length_remaining', None)
        closed = getattr(self.res, 'closed', None)
        if closed is None:
            closed = self.res.isclosed()

        if closed or length_remaining == 0:
            self.res.release_conn()
        else:
            self.res.close()

    def json(self) -> dict:
        data = self.res.data
        if not data:
            return {}
        try:
            if isinstance(data, bytes):
                data = data.decode('utf-8')
            return json.loads(data)
        except (UnicodeDecodeError, json.JSONDecodeError) as e:
            raise ValueError("Invalid JSON response body") from e

    def data(self) -> bytes:
        return self.res.data

    def stream(self) -> Iterator[bytes]:
        return self.res.stream()

    def to_object(self) -> Any:
        return self.json()

    def to_object_list(self) -> Any:
        data = self.json()
        if isinstance(data, list):
            return data
        if isinstance(data, dict) and isinstance(data.get('items'), list):
            return data['items']
        raise TypeError(
            f"expected a JSON array or an object with 'items' list, got {type(data).__name__}")

    def release(self) -> None:
        self.close()

    def raise_for_status(self) -> None:
        if not self.is_ok():
            raise ResponseError(self.status_code, self.res.reason)

    def reason(self) -> str:
        return self.res.reason
