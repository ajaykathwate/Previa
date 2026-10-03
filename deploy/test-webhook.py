"""Send a signed localhost fixture without exposing credentials in process arguments."""
import hashlib
import hmac
import os
from pathlib import Path
import urllib.error
import urllib.request
import uuid

secret = os.environ.get("GITHUB_WEBHOOK_SECRET", "")
if not secret:
    raise SystemExit("Set GITHUB_WEBHOOK_SECRET in .env before testing.")
body = Path(os.environ.get("WEBHOOK_FIXTURE", "internal/webhooks/testdata/ping.json")).read_bytes()
request = urllib.request.Request(
    os.environ.get("WEBHOOK_TEST_URL", "http://localhost:8080/webhooks/github"), data=body, method="POST",
    headers={
        "Content-Type": "application/json",
        "X-GitHub-Event": os.environ.get("WEBHOOK_EVENT", "ping"),
        "X-GitHub-Delivery": os.environ.get("WEBHOOK_DELIVERY_ID", str(uuid.uuid4())),
        "X-Hub-Signature-256": "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest(),
    },
)
try:
    with urllib.request.urlopen(request, timeout=15) as response:
        print(f"HTTP {response.status}: signed fixture accepted")
except urllib.error.HTTPError as error:
    raise SystemExit(f"HTTP {error.code}: fixture rejected; check server configuration/logs.") from None
except urllib.error.URLError:
    raise SystemExit("Could not reach the webhook receiver; start make dev or make run-server.") from None
