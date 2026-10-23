#!/usr/bin/env python3
"""Tests for the tenancy proxy TLS configuration (openssl.conf + listener context)."""
import os
import shutil
import ssl
import subprocess
import tempfile
import unittest

import main

PROXY_DIR = os.path.dirname(os.path.abspath(__file__))
OPENSSL_CONF = os.path.join(PROXY_DIR, "openssl.conf")


def parse_openssl_conf(path):
    """Minimal INI parser: {section: {key: value}}, with '' holding top-level keys."""
    sections = {}
    current = ""
    with open(path) as f:
        for raw in f:
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("[") and line.endswith("]"):
                current = line[1:-1]
                sections.setdefault(current, {})
                continue
            if "=" in line:
                key, value = line.split("=", 1)
                sections.setdefault(current, {})[key.strip()] = value.strip()
    return sections


class OpenSslConfTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.sections = parse_openssl_conf(OPENSSL_CONF)
        cls.system_default = cls.sections["system_default_sect"]

    def test_config_chain_resolves_system_default(self):
        self.assertEqual(self.sections[""]["openssl_conf"], "openssl_init")
        self.assertEqual(self.sections["openssl_init"]["ssl_conf"], "ssl_sect")
        self.assertEqual(self.sections["ssl_sect"]["system_default"], "system_default_sect")

    def test_hybrid_post_quantum_group_is_first(self):
        groups = self.system_default["Groups"].split(":")
        self.assertEqual("X25519MLKEM768", groups[0].lstrip("*"))

    def test_classical_groups_kept_as_fallback(self):
        names = [g.lstrip("*") for g in self.system_default["Groups"].split(":")]
        for fallback in ("X25519", "secp256r1", "X448", "secp384r1"):
            self.assertIn(fallback, names)

    def test_client_hellos_carry_ml_kem_and_x25519_key_shares(self):
        # A '*' prefix marks the groups that send a key_share in outbound
        # ClientHellos; when no group is starred, only the first one does.
        starred = {
            g.lstrip("*")
            for g in self.system_default["Groups"].split(":")
            if g.startswith("*")
        }
        self.assertEqual({"X25519MLKEM768", "X25519"}, starred)

    def test_tls_version_bounds_match_distro_policy(self):
        self.assertEqual("TLSv1.2", self.system_default["TLS.MinProtocol"])
        self.assertEqual("TLSv1.3", self.system_default["TLS.MaxProtocol"])

    def test_cipher_policy_preserved(self):
        self.assertIn("@SECLEVEL=2", self.system_default["CipherString"])
        for suite in (
            "TLS_AES_256_GCM_SHA384",
            "TLS_CHACHA20_POLY1305_SHA256",
            "TLS_AES_128_GCM_SHA256",
        ):
            self.assertIn(suite, self.system_default["Ciphersuites"])


class ServerContextTest(unittest.TestCase):
    def test_listener_enforces_tls13_without_client_certs(self):
        openssl = shutil.which("openssl")
        if openssl is None:
            self.skipTest("openssl CLI not available")
        with tempfile.TemporaryDirectory() as d:
            cert = os.path.join(d, "tls.crt")
            key = os.path.join(d, "tls.key")
            subprocess.run(
                [
                    openssl, "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                    "-days", "1", "-subj", "/CN=test", "-keyout", key, "-out", cert,
                ],
                check=True,
                capture_output=True,
            )
            context = main.create_server_ssl_context(cert, key)
            self.assertEqual(ssl.TLSVersion.TLSv1_3, context.minimum_version)
            self.assertEqual(ssl.CERT_NONE, context.verify_mode)


if __name__ == "__main__":
    unittest.main()
