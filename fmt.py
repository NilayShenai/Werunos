"""Normalize whitespace in Go source files.
Collapses 3+ consecutive blank lines to 1, removes trailing whitespace.
"""

import os, re

def fmt_file(path):
    with open(path, encoding='utf-8', newline='') as f:
        raw = f.read()
    out = raw
    while '\n\n\n' in out:
        out = out.replace('\n\n\n', '\n\n')
    out = re.sub(r'[ \t]+\n', '\n', out)
    out = out.strip() + '\n'
    if out != raw:
        with open(path, 'w', encoding='utf-8', newline='') as f:
            f.write(out)
        return True
    return False

def main():
    changed = 0
    for dirpath, _, files in os.walk('.'):
        for fn in files:
            if fn.endswith('.go'):
                if fmt_file(os.path.join(dirpath, fn)):
                    changed += 1
    print(f'{changed} files trimmed')

if __name__ == '__main__':
    main()
