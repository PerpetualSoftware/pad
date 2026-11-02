#!/usr/bin/env python3
"""Fail-closed CI scope. PRs compare exact head against merge-base(base, head).

GO: everything outside web/. PARITY: any web/ path. WEB: always.
NIX: dependency/packaging policy, deliberately not every derivation source.
Workflows and scripts/ci-* force all jobs. Pushes always run everything.
"""
import fnmatch
import json
import os
from pathlib import Path
import re
import subprocess
import sys

NIX_FILES = {'go.mod', 'go.sum', 'web/package.json', 'web/package-lock.json',
             'web/npm-shrinkwrap.json', 'web/yarn.lock', 'web/pnpm-lock.yaml',
             'web/bun.lock', 'web/bun.lockb'}


def classify(paths):
    outputs = dict(go=False, web=True, nix=False, parity=False)
    for path in paths:
        if path.startswith(('.github/workflows/', 'scripts/ci-')):
            return dict.fromkeys(outputs, True)
        outputs['go'] |= not path.startswith(('web/', 'internal/'))
        outputs['parity'] |= path.startswith('web/')
        outputs['nix'] |= (path.startswith('nix/') or
                           ('/' not in path and fnmatch.fnmatchcase(path, 'flake.*')) or
                           path in NIX_FILES)
    return outputs


def git(*args, cwd=None):
    return subprocess.check_output(['git', *args], cwd=cwd)


def decode_paths(data):
    if not data:
        return []  # A successful, genuinely empty diff is valid.
    if not data.endswith(b'\0'):
        raise ValueError('diff is not NUL terminated')
    paths = [os.fsdecode(p) for p in data[:-1].split(b'\0')]
    if any(not p or p.startswith('/') or any(s in ('.', '..') for s in p.split('/'))
           for p in paths):
        raise ValueError('invalid repository-relative diff path')
    return paths


def pr_paths(base, head, cwd=None):
    for sha in (base, head):
        if not isinstance(sha, str) or not re.fullmatch(r'[0-9a-fA-F]{40}', sha):
            raise ValueError('PR base and head must be exact commit SHAs')
        git('cat-file', '-e', sha + '^{commit}', cwd=cwd)
    ancestor = git('merge-base', base, head, cwd=cwd).decode('ascii').strip()
    if not re.fullmatch(r'[0-9a-fA-F]{40}', ancestor):
        raise ValueError('missing or malformed merge base')
    return decode_paths(git('diff', '--name-only', '-z', '--no-renames', ancestor, head, cwd=cwd))


def has_local_dependencies(go_mod, package):
    # A replacement can introduce extra local inputs. Until classified, keep
    # Nix on for every PR; do the same for npm workspaces/file dependencies.
    if re.search(r'^\s*replace\b', go_mod, re.MULTILINE) or package.get('workspaces'):
        return True
    return any(version.startswith(('file:', 'link:', 'workspace:'))
               for kind in ('dependencies', 'devDependencies', 'optionalDependencies')
               for version in package.get(kind, {}).values())


def for_event(event, payload):
    if event == 'push':
        return dict.fromkeys(('go', 'web', 'nix', 'parity'), True)
    if event != 'pull_request':
        raise ValueError('unsupported CI event: ' + event)
    try:
        pr = payload['pull_request']
        outputs = classify(pr_paths(pr['base']['sha'], pr['head']['sha']))
        root = Path(__file__).resolve().parent.parent
        if has_local_dependencies((root / 'go.mod').read_text(),
                                  json.loads((root / 'web/package.json').read_text())):
            outputs['nix'] = True
        return outputs
    except (KeyError, TypeError) as err:
        raise ValueError('PR event lacks exact base/head SHAs') from err


def main():
    try:
        payload = json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text())
        outputs = for_event(os.environ['GITHUB_EVENT_NAME'], payload)
        text = ''.join(f'{key}={str(value).lower()}\n' for key, value in outputs.items())
        print(text, end='')
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            output.write(text)
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as err:
        print(f'::error::CI scope failed: {err}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
