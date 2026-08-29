from enum import Enum


class RequestMethod(str, Enum):
    """
    请求方法
    """
    GET = "GET"
    POST = "POST"
    PUT = "PUT"
    DELETE = "DELETE"
    PATCH = "PATCH"
