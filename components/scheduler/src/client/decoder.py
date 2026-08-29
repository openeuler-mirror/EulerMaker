import codecs
import io
import json
from typing import Any, Generator, Type, List
from abc import ABC, abstractmethod
from pydantic import BaseModel

from log import warning

from .response import Response

# 缓存累积的最大字符数。单个事件 JSON 远小于该值, 超过说明是无法闭合的非法数据。
_MAX_CACHE_SIZE = 1024 * 1024


class Decoder(ABC):
    '''
    解码器负责解码响应体。
    '''

    def __init__(self, resp: Response):
        self._resp = resp

    @abstractmethod
    def to_object(self, cls_type: Type[BaseModel]) -> BaseModel:
        return None

    @abstractmethod
    def to_object_list(self, cls_type: Type[BaseModel]) -> List[BaseModel]:
        return None

    @abstractmethod
    def to_event(self, cls_type: Type[BaseModel]) -> Generator[BaseModel, None, None]:
        return None

    def is_ok(self) -> bool:
        return self._resp.is_ok()

    def reason(self) -> str:
        return self._resp.reason()

    def raise_for_status(self) -> None:
        self._resp.raise_for_status()

    def close(self) -> None:
        self._resp.close()

    def release(self) -> None:
        self._resp.release()


class DefaultDecoder(Decoder):
    '''
    基于pydantic模型的默认解码器。
    '''

    def __init__(self, resp: Response, delimiter: str = '\n'):
        super().__init__(resp)
        if not delimiter:
            raise ValueError("delimiter must be a non-empty string")
        self._delimiter = delimiter

    def _parse_json_body(self) -> Any:
        '''
        解析响应体为 JSON 对象。
        '''
        if not self._resp.is_ok():
            self._resp.raise_for_status()

        data = self._resp.data()

        if isinstance(data, bytes):
            try:
                data = data.decode('utf-8')
            except UnicodeDecodeError as e:
                raise ValueError(f"Invalid JSON data: {data[:200]!r}") from e

        if isinstance(data, str):
            try:
                return json.loads(data)
            except json.JSONDecodeError as e:
                raise ValueError(f"Invalid JSON data: {data[:200]!r}") from e

        return data

    def to_object(self, cls_type: Type[BaseModel]) -> BaseModel:
        '''
        将响应体解码为对象。
        '''
        self._validate_model_type(cls_type)

        data = self._parse_json_body()
        if not isinstance(data, dict):
            raise TypeError(
                f"expected a JSON object for {cls_type.__name__}, got {type(data).__name__}")
        return cls_type(**data)

    def to_object_list(self, cls_type: Type[BaseModel]) -> List[BaseModel]:
        '''
        将响应体解码为对象列表。

        cls_type 应为单个条目的模型；如果响应体是 {"items": [...]}，
        会提取 items 后逐项转换为 cls_type。
        '''
        self._validate_model_type(cls_type)

        data = self._parse_json_body()

        if isinstance(data, list):
            items = data
        elif isinstance(data, dict):
            items = data.get('items')
            if not isinstance(items, list):
                raise TypeError(
                    f"expected a JSON array or an object with 'items' list for "
                    f"{cls_type.__name__}, got {type(data).__name__}")
        else:
            raise TypeError(
                f"expected a JSON array or an object with 'items' list for "
                f"{cls_type.__name__}, got {type(data).__name__}")

        result = []
        for item in items:
            if not isinstance(item, dict):
                raise TypeError(
                    f"expected JSON objects in list for {cls_type.__name__}, "
                    f"got {type(item).__name__}")
            result.append(cls_type(**item))
        return result

    def to_event(self, cls_type: Type[BaseModel]) -> Generator[BaseModel, None, None]:
        self._validate_model_type(cls_type)
        if not self._resp.is_ok():
            self._resp.raise_for_status()

        buffer = io.StringIO()
        # 使用 replace 错误处理, 忽略非法字符，继续解析后续数据。replace 有可能导致数据丢失，目前业务场景下不会有问题。
        # 如果后续有场景需要严格处理非法字符，可改为 strict 错误处理。
        decoder = codecs.getincrementaldecoder('utf-8')(errors='replace')
        has_binary_data = False

        def feed(data: Any) -> None:
            nonlocal decoder, has_binary_data

            if isinstance(data, bytes):
                has_binary_data = True
                buffer.write(decoder.decode(data))
                return

            if has_binary_data:
                buffer.write(decoder.decode(b'', final=True))
                decoder = codecs.getincrementaldecoder('utf-8')(errors='replace')
                has_binary_data = False

            buffer.write(data)

        try:
            for data in self._resp.stream():
                feed(data)

                while self._delimiter in buffer.getvalue():
                    text = buffer.getvalue()
                    line, text = text.split(self._delimiter, 1)
                    buffer.seek(0)
                    buffer.truncate(0)
                    buffer.write(text)

                    event = self._parse_event_line(line)
                    if event is not None:
                        yield cls_type(**event)

                if len(buffer.getvalue()) > _MAX_CACHE_SIZE:
                    warning(
                        f"DefaultDecoder: buffered fragment exceeds {_MAX_CACHE_SIZE} characters "
                        "without a delimiter, dropping")
                    buffer.seek(0)
                    buffer.truncate(0)
                    decoder = codecs.getincrementaldecoder('utf-8')(errors='replace')
                    has_binary_data = False

            buffer.write(decoder.decode(b'', final=True))
            event = self._parse_event_line(buffer.getvalue())
            if event is not None:
                yield cls_type(**event)
        finally:
            self.release()

    @staticmethod
    def _validate_model_type(cls_type: Type[BaseModel]) -> None:
        if not isinstance(cls_type, type) or not issubclass(cls_type, BaseModel):
            raise TypeError(
                f"cls_type must be a BaseModel subclass, got {cls_type!r}")

    @classmethod
    def _parse_event_line(cls, line: str) -> Any:
        line = line.strip()
        if not line:
            return None

        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            warning(f"DefaultDecoder: dropping invalid JSON event line: {line[:200]!r}")
            return None

        if not isinstance(event, dict):
            warning(f"DefaultDecoder: dropping non-object JSON event: {line[:200]!r}")
            return None

        return event
