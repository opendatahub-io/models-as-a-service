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
# OpenSSL allows '?' (optional group) and '*' (send a key_share) in either order.
_TLS_GROUP_PREFIXES = "*?"


def tls_group_name(token):
    """Group name with optional (?) and key-share (*) prefixes removed."""
    return token.lstrip(_TLS_GROUP_PREFIXES)


def tls_group_sends_key_share(token):
    """True when '*' marks this group for a ClientHello key_share."""
    prefix = token[: len(token) - len(tls_group_name(token))]
    return "*" in prefix


def tls_group_tuples(groups):
    """Split an OpenSSL Groups list into tuples ('/') and groups (':')."""
    return [tuple(part.split(":")) for part in groups.split("/")]


def tls_group_tokens(groups):
    return [token for group_tuple in tls_group_tuples(groups) for token in group_tuple]


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
        cls.crypto_policy = cls.sections["crypto_policy"]
        with open(OPENSSL_CONF) as f:
            cls.directives = [
                line.strip()
                for line in f
                if line.strip() and not line.lstrip().startswith("#")
            ]

    def test_extends_image_config_and_overrides_only_groups(self):
        # The include has to precede every section so the image's openssl_conf
        # assignment stays in the default section.
        include_at = self.directives.index(".include /etc/pki/tls/openssl.cnf")
        first_section = next(i for i, line in enumerate(self.directives) if line.startswith("["))
        self.assertLess(include_at, first_section)
        self.assertNotIn("openssl_conf", self.sections.get("", {}))
        self.assertEqual(["crypto_policy"], list(self.sections))
        self.assertEqual(["Groups"], list(self.crypto_policy))

    def test_does_not_pin_cipher_protocol_or_provider_policy(self):
        pinned = {
            "CipherString",
            "Ciphersuites",
            "TLS.MinProtocol",
            "TLS.MaxProtocol",
            "DTLS.MinProtocol",
            "DTLS.MaxProtocol",
            "SignatureAlgorithms",
            "providers",
            "alg_section",
            "activate",
        }
        for section, values in self.sections.items():
            overlap = pinned.intersection(values)
            self.assertEqual(set(), overlap, section)

    def test_hybrid_post_quantum_group_is_its_own_first_tuple(self):
        tuples = tls_group_tuples(self.crypto_policy["Groups"])
        self.assertEqual(("X25519MLKEM768",), tuple(tls_group_name(g) for g in tuples[0]))
        self.assertGreater(len(tuples), 1)

    def test_classical_groups_kept_as_fallback(self):
        names = [tls_group_name(g) for g in tls_group_tokens(self.crypto_policy["Groups"])]
        for fallback in ("X25519", "secp256r1", "X448", "secp384r1"):
            self.assertIn(fallback, names)

    def test_client_hellos_carry_ml_kem_and_x25519_key_shares(self):
        # '*' (before or after an optional '?') marks groups that send a
        # key_share in outbound ClientHellos. When no group is starred, only
        # the first one does.
        starred = {
            tls_group_name(g)
            for g in tls_group_tokens(self.crypto_policy["Groups"])
            if tls_group_sends_key_share(g)
        }
        self.assertEqual({"X25519MLKEM768", "X25519"}, starred)

    def test_group_prefixes_are_recognized_in_either_order(self):
        for token in ("?*X25519MLKEM768", "*?X25519MLKEM768", "*X25519MLKEM768"):
            self.assertEqual("X25519MLKEM768", tls_group_name(token))
            self.assertTrue(tls_group_sends_key_share(token))
        self.assertEqual("X25519", tls_group_name("?X25519"))
        self.assertFalse(tls_group_sends_key_share("?X25519"))
        self.assertFalse(tls_group_sends_key_share("X25519"))


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
