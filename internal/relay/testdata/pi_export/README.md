What terma's Pi extension (scope `terma-pi` version 2) posted to the relay, one file per
case: `requests` are the OTLP/JSON bodies in the order sent, `hooks` the `terma hook` calls
the extension made. Captured 2026-11-27 on Linux with Pi 1.6.1 against a scripted
OpenAI-compatible provider; paths scrubbed to `/home/user`. Strings that start `CANARY-`,
and `INJECTED-CONTEXT`, are content planted to be looked for: in a prompt, a reply, a
session's name, a tool's arguments and output, a provider's error and a custom message.
