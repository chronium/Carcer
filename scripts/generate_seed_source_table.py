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

content_size = sum(Path(path).stat().st_size for path in sources)
lines = [
    '#include "files.h"',
    "",
    f'_Static_assert({len(sources)}u <= FILE_MAX_COUNT, "too many initial files");',
    (
        f'_Static_assert({content_size}u <= FILE_CONTENT_CAPACITY, '
        '"initial files exceed 64 KiB");'
    ),
]
for path in sources:
    lines.append(
        f'_Static_assert(sizeof("{path}") - 1u <= FILE_MAX_PATH_LENGTH, '
        f'"initial path too long: {path}");'
    )

lines.append("")
for path in sources:
    symbol = binary_symbol(path)
    lines.extend(
        [
            f"extern uint8_t {symbol}_start[];",
            f"extern uint8_t {symbol}_end[];",
        ]
    )

lines.extend(["", "const struct embedded_file initial_files[] = {"])
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
        (
            "const uint32_t initial_file_count = "
            "sizeof(initial_files) / sizeof(initial_files[0]);"
        ),
    ]
)

with output.open("w", encoding="utf-8", newline="\n") as generated:
    generated.write("\n".join(lines) + "\n")
