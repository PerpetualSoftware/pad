"""The merge gate must fail closed on skipped or incomplete dependencies."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest

SCRIPT = Path(__file__).with_name('ci-result.py')
spec = importlib.util.spec_from_file_location('result', SCRIPT)
result = importlib.util.module_from_spec(spec)
spec.loader.exec_module(result)


def needs():
    data = {k: {'result': 'success'} for k in (
        'changes', 'go', 'go-postgres', 'go-server-shards', 'go-web-parity',
        'nix', 'native-smoke', 'web', 'e2e')}
    data['changes']['outputs'] = {'go': 'false', 'web': 'true', 'nix': 'false', 'parity': 'false'}
    return data


class ResultTest(unittest.TestCase):
    def test_expected_skips(self):
        data = needs()
        for job in ('go', 'go-postgres', 'go-server-shards', 'go-web-parity', 'nix'):
            data[job]['result'] = 'skipped'
        self.assertEqual(result.evaluate(data, 'pull_request'), [])

    def test_rejects(self):
        cases = []
        for job, flag in [('go', 'go'), ('go-postgres', 'go'), ('go-server-shards', 'go'),
                          ('nix', 'nix'), ('go-web-parity', 'parity')]:
            data = needs()
            data['changes']['outputs'][flag] = 'true'
            data[job]['result'] = 'skipped'
            cases.append((f'unexpected {job} skip', data, 'pull_request'))
        for job in needs():
            for status in ('failure', 'cancelled', 'unknown', None):
                data = needs()
                data[job]['result'] = status
                cases.append((f'{job} {status}', data, 'pull_request'))
            data = needs()
            del data[job]
            cases.append((f'missing {job}', data, 'pull_request'))
        for job in ('native-smoke', 'web', 'e2e', 'changes'):
            data = needs()
            data[job]['result'] = 'skipped'
            cases.append((f'nonlisted skip {job}', data, 'pull_request'))
        for job in ('go', 'nix', 'go-web-parity'):
            data = needs()
            data[job]['result'] = 'skipped'
            cases.append(('push skip ' + job, data, 'push'))
        for flag in ('go', 'web', 'nix', 'parity'):
            data = needs()
            del data['changes']['outputs'][flag]
            cases.append(('missing output ' + flag, data, 'pull_request'))
        cases.append(('unsupported event', needs(), 'schedule'))
        for name, data, event in cases:
            with self.subTest(name=name):
                self.assertTrue(result.evaluate(data, event))

    def test_push(self):
        data = needs()
        data['changes']['outputs'] = dict.fromkeys(('go', 'web', 'nix', 'parity'), 'true')
        self.assertEqual(result.evaluate(data, 'push'), [])

    def test_cli_contract(self):
        for status, want in [('success', 0), ('failure', 1), ('cancelled', 1), ('skipped', 1)]:
            data=needs()
            data['changes']['outputs']['go']='true'
            data['go']['result']=status
            proc=subprocess.run([sys.executable,str(SCRIPT)], env={**os.environ,
                                'CI_NEEDS':json.dumps(data), 'CI_EVENT_NAME':'pull_request'},
                                capture_output=True, text=True)
            self.assertEqual(proc.returncode,want,proc.stderr)

    def test_cli_malformed(self):
        for raw in ('not json', 'null', '[]', '{}'):
            proc = subprocess.run([sys.executable, str(SCRIPT)], env={**os.environ,
                                  'CI_NEEDS': raw, 'CI_EVENT_NAME': 'pull_request'},
                                  capture_output=True, text=True)
            self.assertNotEqual(proc.returncode, 0, proc.stdout)


if __name__ == '__main__':
    unittest.main()
