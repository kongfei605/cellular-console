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

    def commit(self, message, parents=(), personal_author=False, personal_committer=False, extra_env=None):
        env = dict(self.env)
        if personal_author:
            # Construct a synthetic address without embedding a blocked value
            # in the public test source itself.
            env['GIT_AUTHOR_EMAIL'] = 'ci-fixture' + '@' + 'gmail.com'
        if personal_committer:
            env['GIT_COMMITTER_EMAIL'] = 'ci-fixture' + '@' + 'qq.com'
        env.update(extra_env or {})
        args = ['commit-tree', self.tree]
        for parent in parents:
            args += ['-p', parent]
        return self.git(*args, '-m', message, env=env)

    def check(self):
        with patch.object(privacy, 'ROOT', self.root):
            return privacy.check(history=True)

    def test_temporary_merge_attribution_does_not_block_history(self):
        merge = self.commit(
            'Merge source into base', (self.base, self.head), personal_author=True,
        )
        self.git('update-ref', 'refs/remotes/pull/2/merge', merge)
        self.git('symbolic-ref', 'HEAD', 'refs/remotes/pull/2/merge')
        self.assertTrue(self.check()['passed'])

        # Both the default PR merge checkout and the source checkout pass.
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

    def test_formal_merge_on_main_allows_standard_identity_emails(self):
        merge = self.commit(
            'Merge pull request #2', (self.base, self.head),
            personal_author=True, personal_committer=True,
        )
        self.git('update-ref', 'refs/heads/main', merge)
        self.git('symbolic-ref', 'HEAD', 'refs/heads/main')
        self.assertTrue(self.check()['passed'])

    def test_personal_email_in_merge_message_still_fails(self):
        for message in (
            'Contact ci-fixture' + '@' + 'gmail.com',
            'Merge fixture\n\nauthor Fixture <ci-fixture' + '@' + 'gmail.com> 1 +0000',
        ):
            with self.subTest(message_kind=message.split()[0]):
                merge = self.commit(message, (self.base, self.head), personal_author=True)
                self.git('update-ref', 'refs/heads/main', merge)
                report = self.check()
                self.assertFalse(report['passed'])
                self.assertIn('personal-email', {item['rule'] for item in report['findings']})

    def test_email_in_merge_author_name_still_fails(self):
        merge = self.commit(
            'Merge fixture', (self.base, self.head), personal_author=True,
            extra_env={'GIT_AUTHOR_NAME': 'ci-fixture' + '@' + 'gmail.com'},
        )
        self.git('update-ref', 'refs/heads/main', merge)
        self.assertIn('personal-email', {item['rule'] for item in self.check()['findings']})

    def test_credentials_and_private_addresses_in_merge_metadata_still_fail(self):
        for email, expected in (
            ('ghp_' + 'A' * 36 + '@example.com', 'provider-token'),
            ('fixture@10.23.4.5', 'private-network-address'),
        ):
            with self.subTest(rule=expected):
                merge = self.commit(
                    'Merge fixture', (self.base, self.head), personal_author=True,
                    extra_env={'GIT_COMMITTER_EMAIL': email},
                )
                self.git('update-ref', 'refs/heads/main', merge)
                self.assertIn(expected, {item['rule'] for item in self.check()['findings']})

    def test_personal_email_in_tracked_file_still_fails(self):
        (self.root / 'public.txt').write_text('Contact ci-fixture' + '@' + 'gmail.com\n')
        report = self.check()
        self.assertFalse(report['passed'])
        self.assertIn({'path': 'public.txt', 'rule': 'personal-email'}, report['findings'])


if __name__ == '__main__':
    unittest.main()
