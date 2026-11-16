import importlib.util
from pathlib import Path
import unittest


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


if __name__ == '__main__':
    unittest.main()
