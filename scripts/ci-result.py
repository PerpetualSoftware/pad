#!/usr/bin/env python3
"""Validate the complete CI needs graph; the sole required check is CI result."""
import json
import os
import sys

SCOPED = {'go': 'go', 'go-postgres': 'go', 'go-server-shards': 'go',
          'go-web-parity': 'parity', 'nix': 'nix'}
REQUIRED = {'changes', 'native-smoke', 'web', 'e2e', *SCOPED}


def evaluate(needs, event):
    errors = []
    if event not in ('push', 'pull_request'):
        errors.append(f'unsupported event: {event}')
    if not isinstance(needs, dict):
        return ['needs must be an object']
    if set(needs) != REQUIRED:
        errors.append(f'dependency mismatch: missing={sorted(REQUIRED - set(needs))}, '
                      f'extra={sorted(set(needs) - REQUIRED)}')
    changes = needs.get('changes', {})
    outputs = changes.get('outputs', {}) if isinstance(changes, dict) else {}
    if not isinstance(outputs, dict):
        outputs = {}
    for key in ('go', 'web', 'nix', 'parity'):
        if outputs.get(key) not in ('true', 'false'):
            errors.append(f'missing/invalid changes output: {key}')
        elif event == 'push' and outputs[key] != 'true':
            errors.append(f'push must enable {key}')
    for job in sorted(REQUIRED):
        record = needs.get(job)
        status = record.get('result') if isinstance(record, dict) else None
        if status == 'success':
            continue
        if (status == 'skipped' and event == 'pull_request' and
                job in SCOPED and outputs.get(SCOPED[job]) == 'false'):
            continue
        errors.append(f'{job}: {status!r} is not an authorized result')
    return errors


def main():
    try:
        errors = evaluate(json.loads(os.environ['CI_NEEDS']), os.environ['CI_EVENT_NAME'])
    except (KeyError, ValueError) as err:
        errors = [str(err)]
    for error in errors:
        print('::error::' + error, file=sys.stderr)
    if not errors:
        print('Every required CI dependency passed or was correctly scoped out.')
    return bool(errors)


if __name__ == '__main__':
    sys.exit(main())
