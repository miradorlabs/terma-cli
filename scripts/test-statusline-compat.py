#!/usr/bin/env python3
"""Compare real third-party renderers directly and through terma, without installing them.

Download dependencies separately; this runner performs no package installation.
See docs/STATUSLINE-COMPATIBILITY.md for pinned revisions and commands.
"""
import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile


def write_json(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(data, ensure_ascii=False, indent=2) + '\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--terma', type=Path, required=True)
    parser.add_argument('--ccstatusline-js', type=Path, required=True)
    parser.add_argument('--claude-statusline-root', type=Path, required=True)
    parser.add_argument('--report', type=Path)
    args = parser.parse_args()
    binary = args.terma.resolve()
    cc = args.ccstatusline_js.resolve()
    upstream = args.claude_statusline_root.resolve()
    node = shutil.which('node')
    assert node and shutil.which('jq'), 'node and jq are required'
    results = []
    with tempfile.TemporaryDirectory(prefix='terma statusline compatibility ') as tmp:
        root = Path(tmp)
        claude = root / 'claude config'
        claude.mkdir()
        terma = root / 'terma config'
        terma.mkdir()
        work = root / '工作 tree with spaces'
        work.mkdir()
        # Prevent background flushes from attempting delivery: the scratch repo
        # has no project/key. Renderer configs and transcripts are synthetic.
        subprocess.run(['git', 'init', '-q', str(work)], check=True)
        transcript = root / 'synthetic transcript.jsonl'
        transcript.write_text(json.dumps({'type': 'assistant', 'message': {
            'role': 'assistant', 'usage': {'input_tokens': 28000,
            'cache_creation_input_tokens': 16000, 'cache_read_input_tokens': 40000,
            'output_tokens': 1200}}}, separators=(',', ':')) + '\n')
        payload = {'session_id': 'compat-session', 'prompt_id': 'compat-prompt',
            'cwd': str(work), 'workspace': {'current_dir': str(work), 'project_dir': str(work)},
            'transcript_path': str(transcript), 'model': {'id': 'claude-opus-4-6', 'display_name': 'Opus 4.6'},
            'version': '2.1.272', 'cost': {'total_cost_usd': 1.234, 'total_duration_ms': 45000},
            'context_window': {'context_window_size': 200000, 'used_percentage': 42,
                'remaining_percentage': 58, 'current_usage': {'input_tokens': 28000,
                'cache_creation_input_tokens': 16000, 'cache_read_input_tokens': 40000}},
            'rate_limits': {'five_hour': {'used_percentage': 98, 'resets_at': 1900000000},
                'seven_day': {'used_percentage': 61, 'resets_at': 1900600000}}}
        body = json.dumps(payload, ensure_ascii=False).encode()
        env = os.environ.copy()
        for key in list(env):
            if key.startswith('TERMA_'):
                del env[key]
        env.update(TERMA_CONFIG_DIR=str(terma), CLAUDE_CONFIG_DIR=str(claude),
                   COLORTERM='truecolor', TERM='xterm-256color', NO_COLOR='1')

        def compare(name, command, colored=False, width=80):
            case_env = dict(env, COLUMNS=str(width))
            if colored:
                case_env.pop('NO_COLOR', None)
            previous = {'type': 'command', 'command': command, 'padding': 2,
                        'refreshInterval': 5, 'hideVimModeIndicator': True}
            # Exercise renderer lookup using the production record format.
            write_json(terma / 'statusline.json', {str(claude / 'settings.json'):
                       {'installed': {'type': 'command', 'command': 'terma hook statusline'},
                        'previous': previous if command else None}})
            direct = subprocess.run(['/bin/sh', '-c', command or 'exit 0'], input=body,
                                    cwd=work, env=case_env, capture_output=True, timeout=30)
            wrapped = subprocess.run([str(binary), 'hook', 'statusline'], input=body,
                                     cwd=work, env=case_env, capture_output=True, timeout=30)
            marker = b'\x1b[38;2;139;108;255mt\x1b[0m ' if colored else b't '
            expected = marker + direct.stdout if direct.stdout else b''
            assert direct.returncode == 0, (name, 'upstream renderer failed', direct.stderr.decode(errors='replace'))
            assert wrapped.returncode == direct.returncode, (name, 'exit status', wrapped.stderr)
            assert wrapped.stderr == direct.stderr, (name, 'stderr', direct.stderr, wrapped.stderr)
            assert wrapped.stdout == expected, (name, 'stdout differs', direct.stdout, wrapped.stdout)
            # Disabled capture must reproduce the unwrapped renderer exactly.
            disabled = subprocess.run([str(binary), 'hook', 'statusline'], input=body,
                cwd=work, env=dict(case_env, TERMA_HOOKS='0'), capture_output=True, timeout=30)
            assert (disabled.stdout, disabled.stderr, disabled.returncode) == (direct.stdout, direct.stderr, direct.returncode), (name, 'disabled capture changed output')
            results.append({'case': name, 'colored_marker': colored, 'columns': width,
                'renderer_bytes': len(direct.stdout), 'ansi': b'\x1b[' in direct.stdout,
                'osc8': b'\x1b]8;' in direct.stdout, 'lines': direct.stdout.count(b'\n'),
                'sha256': hashlib.sha256(direct.stdout).hexdigest(), 'passed': True})
            print(f'PASS {name} ({len(direct.stdout)} renderer bytes)', flush=True)

        compare('no previous renderer', '')
        compare('conditionally empty renderer', 'true')
        base = {'version': 4, 'colorLevel': 3, 'defaultSeparator': '|', 'globalBold': True,
                'lines': [[{'id': 'm', 'type': 'model', 'color': 'cyan'},
                           {'id': 'c', 'type': 'context-percentage', 'color': 'yellow'}]],
                'powerline': {'enabled': False}}
        cases = [('standard', base)]
        power = copy.deepcopy(base)
        power['powerline'] = {'enabled': True, 'separators': [''], 'startCaps': [''],
                            'endCaps': [''], 'autoAlign': True}
        power['lines'][0][0]['backgroundColor'] = 'blue'
        power['lines'][0][1]['backgroundColor'] = 'magenta'
        cases.append(('powerline', power))
        multi = copy.deepcopy(power)
        multi['lines'].append([{'id': 'link', 'type': 'link', 'color': 'green',
            'metadata': {'url': 'https://example.test/a?b=c', 'text': '文档 λ 👩🏽‍💻'}},
            {'id': 'dir', 'type': 'current-working-dir', 'color': 'magenta'}])
        cases.append(('multiline hyperlinks unicode', multi))
        for label, config in cases:
            config_path = root / ('cc ' + label + '.json')
            write_json(config_path, config)
            command = shlex.join([node, str(cc), '--config', str(config_path)])
            for colored in (False, True):
                for width in (40, 120):
                    compare(f'ccstatusline {label} color={colored} width={width}', command, colored, width)

        # Run the actual shell project from a scratch copy. Disable provider usage
        # widgets so this is a renderer compatibility test, not a billing/network test.
        shell_root = root / 'shell statusline with spaces'
        shutil.copytree(upstream / 'src', shell_root / 'src')
        (shell_root / 'data').mkdir()
        config = json.loads((upstream / 'config/config.example.json').read_text())
        config['sections'] = {key: False for key in config['sections'] if key.startswith('show_')}
        config['sections'].update(show_directory=True, show_context=True, weekly_display_mode='usage')
        config['paths']['claude_projects'] = str(root / 'nonexistent projects')
        command = shlex.join(['/bin/bash', str(shell_root / 'src/statusline.sh')])
        for label, changes in [('default styling', {}), ('truecolor', {'bright_orange': '\\033[38;2;255;120;50m', 'orange': '\\033[38;2;255;120;50m'}),
                               ('256-color bold', {'bright_orange': '\\033[1;38;5;208m', 'orange': '\\033[1;38;5;208m'})]:
            variant = copy.deepcopy(config)
            variant['colors'].update(changes)
            write_json(shell_root / 'config/config.json', variant)
            for colored in (False, True):
                compare('claude-statusline ' + label + f' color={colored}', command, colored)
        # Exercise the shell renderer's actual multi-layer progress bars with
        # deterministic ccusage responses. No npm invocation or account reads.
        fake_bin = root / 'fixture tools'
        fake_bin.mkdir()
        usage = root / 'usage.json'
        stub = fake_bin / 'npx'
        stub.write_text('#!/bin/sh\ncat "$TERMA_COMPAT_USAGE_FILE"\n')
        stub.chmod(0o700)
        env['PATH'] = str(fake_bin) + os.pathsep + env['PATH']
        env['TERMA_COMPAT_USAGE_FILE'] = str(usage)
        config['sections'].update(show_five_hour_window=True, show_weekly=True)
        config['tracking']['weekly_scheme'] = 'ccusage'
        for cost in (30, 100, 180):
            write_json(usage, {'blocks': [{'costUSD': cost, 'projection': {'totalCost': cost+40}}],
                               'weekly': [{'totalCost': 123}]})
            write_json(shell_root / 'config/config.json', config)
            compare(f'claude-statusline progress layers cost={cost}', command, True)
    report = {'ccstatusline_package': json.loads((cc.parent.parent / 'package.json').read_text())['version'],
              'ccstatusline_js_sha256': hashlib.sha256(cc.read_bytes()).hexdigest(), 'claude_statusline_revision': subprocess.check_output(
        ['git', '-C', str(upstream), 'rev-parse', 'HEAD'], text=True).strip(), 'cases': results}
    if args.report:
        write_json(args.report, report)
    print(f'{len(results)} compatibility cases passed; each also checked with capture disabled.')


if __name__ == '__main__':
    main()
