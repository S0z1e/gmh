#!/usr/bin/env python3
"""Run the Go harness with the OpenConsole credential from the vault.

The credential never appears in a file, an argument, or a log. This script
fetches only an ``hsurr:`` surrogate from authd, passes it to the harness
as OPENAI_API_KEY in the child process environment, and never prints it.
Sentinel/authd swaps the surrogate for the real key on egress to
www.openconsole.cn, which is a registered host for custom.openconsole.

Usage:
    python3 scripts/run_with_vault_key.py --api chat|responses --root DIR --task "..."
"""
import argparse
import os
import subprocess
import sys

sys.path.insert(0, "/opt/muse/skills/custom-connector-setup/bin")
from dynamic_credentials import dynamic_credential_entry


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--api", default="chat", choices=["chat", "responses"])
    parser.add_argument("--root", required=True)
    parser.add_argument("--task", required=True)
    parser.add_argument("--binary", default="/tmp/harness")
    parser.add_argument("--model", default="deepseek-v4.1-flash")
    args = parser.parse_args()

    entry = dynamic_credential_entry("custom.openconsole")
    surrogate = str(entry["surrogate"]).strip()
    if not surrogate.startswith("hsurr:"):
        raise RuntimeError("authd did not return a surrogate")

    env = dict(os.environ)
    env["OPENAI_API_KEY"] = surrogate
    env["OPENAI_BASE_URL"] = "https://www.openconsole.cn/v1"
    env["OPENAI_MODEL"] = args.model
    env["OPENAI_API_MODE"] = args.api

    cmd = [
        args.binary,
        "-api", args.api,
        "-p", args.task,
        "-root", args.root,
        "-mode", "auto",
    ]
    result = subprocess.run(cmd, env=env, timeout=180, capture_output=False)
    return result.returncode


if __name__ == "__main__":
    raise SystemExit(main())
