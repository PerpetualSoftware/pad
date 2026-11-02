"""Path policy and real git diff regression cases (including hostile filenames)."""
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).with_name('ci-scope.py')
spec = importlib.util.spec_from_file_location('scope', SCRIPT)
scope = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scope)


class ScopeTest(unittest.TestCase):
    def test_paths(self):
        for path, want in [
            ('web/static/a', (False, True, False, True)),
            ('web/src/a.ts', (False, True, False, True)),
            ('internal/store/a.go', (True, True, False, False)),
            ('go.mod', (True, True, True, False)),
            ('go.sum', (True, True, True, False)),
            ('.github/workflows/new.yml', (True, True, True, True)),
            ('scripts/ci-web-parity.txt', (True, True, True, True)),
            ('scripts/ci-web-census/main.go', (True, True, True, True)),
            ('README.md', (True, True, False, False)),
            ('nix/package.nix', (True, True, True, False)),
            ('flake.lock', (True, True, True, False)),
            ('web/package-lock.json', (False, True, True, True)),
            ('web/package.json', (False, True, True, True)),
            ('web/file with spaces\nand newline', (False, True, False, True)),
        ]:
            with self.subTest(path=path):
                got = scope.classify([path])
                self.assertEqual(tuple(got[k] for k in ('go', 'web', 'nix', 'parity')), want)

    def test_local_dependency_guard(self):
        cases = [
            ('module example.invalid/p\n', {}, False),
            ('replace example.invalid/dep => ../dep\n', {}, True),
            ('replace (\n example.invalid/dep => ./dep\n)\n', {}, True),
            ('module example.invalid/p\n', {'workspaces': ['packages/*']}, True),
            ('module example.invalid/p\n', {'dependencies': {'dep': 'file:../dep'}}, True),
            ('module example.invalid/p\n', {'devDependencies': {'dep': 'workspace:*'}}, True),
        ]
        for go_mod, package, want in cases:
            with self.subTest(go_mod=go_mod, package=package):
                self.assertEqual(scope.has_local_dependencies(go_mod, package), want)

    def test_push_and_errors(self):
        self.assertTrue(all(scope.for_event('push', {}).values()))
        for event, payload in [('schedule', {}), ('pull_request', {}),
                               ('pull_request', {'pull_request': {'base': {'sha': 'bad'}, 'head': {'sha': 'bad'}}})]:
            with self.subTest(event=event, payload=payload), self.assertRaises(ValueError):
                scope.for_event(event, payload)
        for data in [b'file', b'\0', b'../outside\0', b'/absolute\0', b'a\0\0']:
            with self.subTest(data=data), self.assertRaises(ValueError):
                scope.decode_paths(data)
        self.assertEqual(scope.decode_paths(b''), [])
        for returns in (
                [subprocess.CalledProcessError(1, 'cat-file')],
                [b'', b'', subprocess.CalledProcessError(1, 'merge-base')],
                [b'', b'', b'not-a-sha\n'],
                [b'', b'', b'a'*40+b'\n', subprocess.CalledProcessError(1, 'diff')]):
            with self.subTest(returns=returns), patch.object(scope, 'git', side_effect=returns):
                with self.assertRaises((subprocess.CalledProcessError, ValueError)):
                    scope.pr_paths('a'*40, 'b'*40)

    def test_merge_base_deletions_and_rename(self):
        with tempfile.TemporaryDirectory() as tmp:
            def git(*args):
                return subprocess.check_output(['git', '-C', tmp, *args]).decode().strip()
            def commit():
                git('add', '-A')
                git('-c', 'user.name=Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'fixture')
                return git('rev-parse', 'HEAD')
            git('init', '-q')
            root = Path(tmp)
            (root / 'web').mkdir()
            (root / 'web' / 'old\n name').write_text('original')
            ancestor = commit()
            (root / 'base-only.go').write_text('base')
            base = commit()
            # Detached test-fixture history only; never changes the task worktree.
            git('checkout', '-q', '--detach', ancestor)
            (root / 'web' / 'old\n name').rename(root / 'new name.go')
            commit()
            (root / 'web' / 'other space').write_text('second commit')
            head = commit()
            paths = scope.pr_paths(base, head, cwd=tmp)
            self.assertEqual(set(paths), {'web/old\n name', 'new name.go', 'web/other space'})
            self.assertTrue(scope.classify(paths)['go'])
            self.assertTrue(scope.classify(paths)['parity'])
            with self.assertRaises(subprocess.CalledProcessError):
                scope.pr_paths('f'*40, head, cwd=tmp)


if __name__ == '__main__':
    unittest.main()
