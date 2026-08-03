#!/usr/bin/env python3
import getpass
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path


REPO_DIR = Path(__file__).resolve().parents[1]
REMOTE_NAME = "gitlab"
REMOTE_URL = "https://gitlab.com/jexiptv07/stremcore.git"
BRANCH = "main"


def run(cmd, env=None):
    print("+ " + " ".join(cmd))
    subprocess.run(cmd, cwd=REPO_DIR, env=env, check=True)


def run_capture(cmd, env=None):
    print("+ " + " ".join(cmd))
    return subprocess.run(
        cmd,
        cwd=REPO_DIR,
        env=env,
        check=False,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )


def remote_exists(name):
    result = subprocess.run(
        ["git", "remote", "get-url", name],
        cwd=REPO_DIR,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    return result.returncode == 0


def main():
    username = os.environ.get("GITLAB_USERNAME") or input("GitLab username: ").strip()
    token = os.environ.get("GITLAB_TOKEN") or getpass.getpass("GitLab token: ")

    overwrite = "--overwrite" in sys.argv or "--force-with-lease" in sys.argv

    if not username or not token:
        print("Falta username o token.", file=sys.stderr)
        return 1

    if remote_exists(REMOTE_NAME):
        run(["git", "remote", "set-url", REMOTE_NAME, REMOTE_URL])
    else:
        run(["git", "remote", "add", REMOTE_NAME, REMOTE_URL])

    askpass = tempfile.NamedTemporaryFile("w", delete=False, prefix="gitlab-askpass-", suffix=".sh")
    askpass_path = Path(askpass.name)
    try:
        askpass.write(
            "#!/bin/sh\n"
            "case \"$1\" in\n"
            "  *Username*) printf '%s\\n' \"$GITLAB_USERNAME\" ;;\n"
            "  *Password*) printf '%s\\n' \"$GITLAB_TOKEN\" ;;\n"
            "  *) printf '\\n' ;;\n"
            "esac\n"
        )
        askpass.close()
        askpass_path.chmod(0o700)

        env = os.environ.copy()
        env["GITLAB_USERNAME"] = username
        env["GITLAB_TOKEN"] = token
        env["GIT_ASKPASS"] = str(askpass_path)
        env["GIT_TERMINAL_PROMPT"] = "0"

        run(["git", "branch", "-M", BRANCH], env=env)
        result = run_capture(["git", "push", "-u", REMOTE_NAME, BRANCH], env=env)
        if result.returncode == 0:
            print(result.stdout, end="")
            return 0

        print(result.stdout, end="")
        rejected = bool(re.search(r"\[rejected\].*fetch first", result.stdout))
        if not rejected:
            return result.returncode

        if not overwrite:
            answer = input(
                "GitLab ya tiene commits propios. Sobrescribir GitLab con este repo? [y/N]: "
            ).strip().lower()
            overwrite = answer in {"y", "yes", "s", "si", "sí"}

        if not overwrite:
            print("Cancelado. No se modifico el repo remoto de GitLab.")
            return 1

        run(["git", "push", "--force-with-lease", "-u", REMOTE_NAME, BRANCH], env=env)
        return 0
    finally:
        try:
            askpass_path.unlink()
        except FileNotFoundError:
            pass


if __name__ == "__main__":
    raise SystemExit(main())
