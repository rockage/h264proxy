"""No sudo, SSH, real key or network changes: exercise launcher argument wiring."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class PerformanceArgumentsTest(unittest.TestCase):
    def invoke(self, values, body='h264_performance_args; printf "%s\\n" "${H264_PERF_ARGS[@]}"'):
        env = {k: v for k, v in os.environ.items() if not k.startswith('H264_')}
        env.update(values)
        return subprocess.run(['bash', '-c', 'set -e; source scripts/client_config.sh; ' + body],
                              cwd=ROOT, env=env, capture_output=True, text=True)

    def test_defaults_and_explicit_thresholds(self):
        result = self.invoke({})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines(), ['-dc-high-water', '0', '-dc-low-water', '0', '-stream-receive-window', '0'])
        values = dict(H264_DC_HIGH_WATER='1048576', H264_DC_LOW_WATER='524288', H264_STREAM_RECEIVE_WINDOW='1048576')
        result = self.invoke(values)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines()[1::2], ['1048576', '524288', '1048576'])

    def test_invalid_settings_fail(self):
        for values in ({'H264_DC_HIGH_WATER': '1'}, {'H264_DC_LOW_WATER': '1'},
                       {'H264_STREAM_RECEIVE_WINDOW': '-1'}, {'H264_DC_HIGH_WATER': 'x'},
                       {'H264_DC_HIGH_WATER': '1048576', 'H264_DC_LOW_WATER': '1048576'}):
            with self.subTest(values=values):
                self.assertNotEqual(self.invoke(values).returncode, 0)

    def test_client_file_and_environment_override(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'client.env'
            path.write_text('H264_DC_HIGH_WATER=1048576\nH264_DC_LOW_WATER=524288\nH264_STREAM_RECEIVE_WINDOW=1048576\n')
            result = self.invoke({'H264_CONFIG': str(path), 'H264_STREAM_RECEIVE_WINDOW': '2097152'},
                                 'h264_load_client_config "$PWD"; h264_performance_args; printf "%s\\n" "${H264_PERF_ARGS[@]}"')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.splitlines()[1::2], ['1048576', '524288', '2097152'])


if __name__ == '__main__':
    unittest.main()
