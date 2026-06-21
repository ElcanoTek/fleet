"""Test fixture: a JSON-RPC stdio server that never answers.

Used to exercise call cancellation: the caller's context expires while the
read is outstanding, which must poison the transport for subsequent calls.
"""

import sys
import time


def main() -> None:
    while True:
        line = sys.stdin.readline()
        if not line:
            break
        time.sleep(3600)


if __name__ == "__main__":
    main()
