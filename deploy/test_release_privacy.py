import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / 'scripts' / 'check_release.py'
SPEC = importlib.util.spec_from_file_location('release_privacy', SCRIPT)
privacy = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(privacy)


class PublicNetworkPrivacyTests(unittest.TestCase):
    def test_private_addresses_blocked_in_public_context(self):
        for address in ('10.23.4.5', '172.16.1.2', '172.31.1.2', '192.168.1.9'):
            with self.subTest(address=address):
                data = f'https://{address}:8788/#network'.encode()
                self.assertIn('private-network-address', privacy.findings(data, True))

    def test_reserved_examples_and_loopback_allowed(self):
        for address in ('192.0.2.10', '198.51.100.1', '203.0.113.1', '127.0.0.1'):
            with self.subTest(address=address):
                self.assertEqual(privacy.findings(address.encode(), True), [])

    def test_internal_address_fixtures_allowed_in_unit_tests(self):
        self.assertEqual(privacy.findings(b'http://10.23.4.5/sms'), [])

    def test_documentation_and_example_config_context(self):
        for name in ('README.md', 'docs/deployment.md', 'configs/host.example.json'):
            self.assertTrue(privacy.public_network_context(name))
        self.assertFalse(privacy.public_network_context('forwarding_test.go'))


class HistoryCheckoutTests(unittest.TestCase):
    """Exercise real Git history, including GitHub's extra PR merge object."""

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.env = {
            **os.environ,
            'GIT_AUTHOR_NAME': 'CI Fixture',
            'GIT_AUTHOR_EMAIL': 'fixture@users.noreply.github.com',
            'GIT_COMMITTER_NAME': 'CI Fixture',
            'GIT_COMMITTER_EMAIL': 'fixture@users.noreply.github.com',
        }
        self.git('init', '-q', '-b', 'main')
        (self.root / 'public.txt').write_text('Synthetic release fixture\n')
        self.git('add', 'public.txt')
        self.tree = self.git('write-tree')
        self.base = self.commit('chore: base fixture')
        self.head = self.commit('fix: source fixture', (self.base,))
        self.git('update-ref', 'refs/heads/main', self.base)
        self.git('update-ref', 'refs/heads/change', self.head)
        self.git('symbolic-ref', 'HEAD', 'refs/heads/change')

    def git(self, *args, env=None):
        return subprocess.check_output(
            ['git', *args], cwd=self.root, env=env or self.env,
            text=True, stderr=subprocess.PIPE,
        ).strip()

    def commit(self, message, parents=(), personal_author=False):
        env = dict(self.env)
        if personal_author:
            # Construct a synthetic address without embedding a blocked value
            # in the public test source itself.
            env['GIT_AUTHOR_EMAIL'] = 'ci-fixture' + '@' + 'gmail.com'
        args = ['commit-tree', self.tree]
        for parent in parents:
            args += ['-p', parent]
        return self.git(*args, '-m', message, env=env)

    def check(self):
        with patch.object(privacy, 'ROOT', self.root):
            return privacy.check(history=True)

    def test_source_history_passes_when_temporary_merge_is_not_fetched(self):
        merge = self.commit(
            'Merge source into base', (self.base, self.head), personal_author=True,
        )
        self.git('update-ref', 'refs/remotes/pull/2/merge', merge)
        self.git('symbolic-ref', 'HEAD', 'refs/remotes/pull/2/merge')
        self.assertFalse(self.check()['passed'])

        # A source-head checkout fetches branch history, without the platform's
        # extra merge ref. No email rule or real source commit is excluded.
        self.git('symbolic-ref', 'HEAD', 'refs/heads/change')
        self.git('update-ref', '-d', 'refs/remotes/pull/2/merge')
        self.assertTrue(self.check()['passed'])

    def test_personal_email_in_actual_source_history_still_fails(self):
        source = self.commit('fix: source author fixture', (self.head,), personal_author=True)
        self.git('update-ref', 'refs/heads/change', source)
        report = self.check()
        self.assertFalse(report['passed'])
        self.assertIn('personal-email', {item['rule'] for item in report['findings']})
        self.assertNotIn('ci-fixture' + '@' + 'gmail.com', str(report))


if __name__ == '__main__':
    unittest.main()
