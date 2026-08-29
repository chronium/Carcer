import string
import sys
from pathlib import Path


if len(sys.argv) < 3:
    raise SystemExit("usage: generate_seed_source_table.py OUTPUT SOURCE...")


def binary_symbol(path):
    return "_binary_" + "".join(
        character if character.isalnum() else "_" for character in path
    )


output = Path(sys.argv[1])
sources = sys.argv[2:]
allowed = set(string.ascii_letters + string.digits + "._/-")
if not sources or any(not path or not set(path) <= allowed for path in sources):
    raise SystemExit(
        "seed source paths must use ASCII letters, digits, '.', '_', '/', or '-'"
    )

lines = ['#include "files.h"', ""]
for path in sources:
    symbol = binary_symbol(path)
    lines.extend(
        [
            f"extern uint8_t {symbol}_start[];",
            f"extern uint8_t {symbol}_end[];",
        ]
    )

lines.extend(["", "struct file files[] = {"])
for path in sources:
    symbol = binary_symbol(path)
    lines.extend(
        [
            "    {",
            f'        (const uint8_t *)"{path}",',
            f'        sizeof("{path}") - 1,',
            f"        {symbol}_start,",
            f"        {symbol}_end,",
            "    },",
        ]
    )
lines.extend(
    [
        "};",
        "",
        "const uint32_t file_count = sizeof(files) / sizeof(files[0]);",
    ]
)

with output.open("w", encoding="utf-8", newline="\n") as generated:
    generated.write("\n".join(lines) + "\n")
