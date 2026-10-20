import argparse
import json
import sys
import time
import uuid
from datetime import datetime, timedelta, timezone
from decimal import Decimal
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://localhost:8080")
    args = parser.parse_args()
    base = args.url.rstrip("/")

    def call(method: str, path: str, token: str = "", data=None, status: int = 200):
        headers = {}
        if token:
            headers["Authorization"] = "Bearer " + token
        raw = None
        if data is not None:
            raw = json.dumps(data).encode()
            headers["Content-Type"] = "application/json"
        req = Request(base + path, data=raw, headers=headers, method=method)
        try:
            res = urlopen(req, timeout=15)
        except HTTPError as err:
            res = err
        with res:
            text = res.read()
            value = json.loads(text, parse_float=Decimal) if text else None
            if res.status != status:
                raise RuntimeError(f"{method} {path}: {res.status}, expected {status}: {text.decode()}")
            if not res.headers.get("X-Request-Id"):
                raise RuntimeError("X-Request-Id missing")
            print(f"{method} {path}: {res.status}")
            return value

    for attempt in range(30):
        try:
            call("GET", "/products", status=401)
            break
        except URLError:
            if attempt == 29:
                raise
            time.sleep(1)

    suffix = uuid.uuid4().hex[:12]
    password = "smoke-password-123"

    def user(role: str):
        credentials = {"email": f"{role.lower()}-{suffix}@example.test", "password": password}
        call("POST", "/auth/register", data={**credentials, "role": role}, status=201)
        return call("POST", "/auth/login", data=credentials)

    seller = user("SELLER")["access_token"]
    pair = user("USER")
    buyer = pair["access_token"]
    product = {"name": "Book", "description": None, "price": 199.9, "stock": 10,
               "category": "smoke-" + suffix, "status": "ACTIVE"}
    denied = call("POST", "/products", buyer, product, 403)
    assert denied["error_code"] == "ACCESS_DENIED"
    p = call("POST", "/products", seller, product, 201)
    product_path = f"/products/{p['id']}"
    now = datetime.now(timezone.utc)
    promo = call("POST", "/promo-codes", seller, {
        "code": "SM_" + suffix.upper(), "discount_type": "PERCENTAGE", "discount_value": 10,
        "min_order_amount": 300, "max_uses": 1,
        "valid_from": (now - timedelta(minutes=1)).isoformat(),
        "valid_until": (now + timedelta(days=1)).isoformat(),
    }, 201)
    order = call("POST", "/orders", buyer, {
        "items": [{"product_id": p["id"], "quantity": 2}], "promo_code": promo["code"],
    }, 201)
    assert order["total_amount"] == Decimal("359.82")
    assert order["discount_amount"] == Decimal("39.98")
    assert call("GET", product_path, buyer)["stock"] == 8
    call("PUT", product_path, seller, {**product, "price": 299.9, "stock": 8})
    order_path = f"/orders/{order['id']}"
    order = call("PUT", order_path, buyer, {"items": [{"product_id": p["id"], "quantity": 3}]})
    assert order["items"][0]["price_at_order"] == Decimal("199.90")
    assert order["total_amount"] == Decimal("539.73")
    assert order["discount_amount"] == Decimal("59.97")
    assert call("POST", order_path + "/cancel", buyer)["status"] == "CANCELED"
    assert call("GET", product_path, buyer)["stock"] == 10
    call("DELETE", product_path, seller, status=204)
    assert call("GET", product_path, buyer)["status"] == "ARCHIVED"
    fresh = call("POST", "/auth/refresh", data={"refresh_token": pair["refresh_token"]})
    bad = call("POST", "/auth/refresh", data={"refresh_token": pair["refresh_token"]}, status=401)
    assert bad["error_code"] == "REFRESH_TOKEN_INVALID"
    call("GET", "/products?category=" + product["category"], fresh["access_token"])
    print("OK: роли, товары, снимок цены, скидка, возврат остатков и refresh")
    print(f"Тестовые данные оставлены в БД: product_id={p['id']}, order_id={order['id']}")


if __name__ == "__main__":
    try:
        main()
    except (URLError, RuntimeError, AssertionError, KeyError) as err:
        print(f"FAILED: {type(err).__name__}: {err}", file=sys.stderr)
        sys.exit(1)
