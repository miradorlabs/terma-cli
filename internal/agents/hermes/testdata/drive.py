# Drives the rendered plugin (termaplug.py in argv[1]) with a successful and a failed API call,
# and prints each span it would export as a JSON line: {name, error, attrs}, then whether any
# export held the failure's private text. The export is captured, never sent.
import json
import sys

sys.path.insert(0, sys.argv[1])
import termaplug as plugin  # noqa: E402

sent = []
plugin._post = lambda path, body: sent.append(body)
plugin.threading.Thread = lambda **_: type("Idle", (), {"start": lambda self: None})()
hooks = {}
plugin.register(type("Ctx", (), {"register_hook": lambda self, name, fn: hooks.__setitem__(name, fn)})())

hooks["post_api_request"](session_id="s1", model="m", response_model="m-served", provider="openai", usage=None,
                          started_at=1.0, ended_at=2.0, finish_reason="stop", api_request_id="t1:api:1", turn_id="t1")
hooks["api_request_error"](session_id="s1", model="m", provider="openai", started_at=3.0, ended_at=4.0,
                           api_request_id="t1:api:2", turn_id="t1", status_code=429, reason="rate_limit",
                           error={"type": "RateLimitError", "message": "PRIVATE prompt text"},
                           request={"messages": ["PRIVATE"]})
plugin._flush()

for body in sent:
    for rs in body.get("resourceSpans", []):
        for ss in rs["scopeSpans"]:
            for sp in ss["spans"]:
                attrs = {a["key"]: str(list(a["value"].values())[0]) for a in sp["attributes"]}
                print(json.dumps({"name": sp["name"], "error": sp.get("status", {}).get("code") == 2, "attrs": attrs}))
print(json.dumps({"private": "PRIVATE" in json.dumps(sent)}))
