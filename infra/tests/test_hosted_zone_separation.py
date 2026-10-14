"""DES-HOR-601-03: the in-cluster Cloudflare token never reaches the mail zone.

Source guards only: no Terraform backend, credentials or cloud API calls. The
token's permission-group lookups are list-nested Cloudflare data that Terraform
test mocks cannot express, so its zone scope is guarded here; the rest of the
hosted-zone wiring is asserted by tests/hosted_ingress.tftest.hcl.
"""

from pathlib import Path
import re
import unittest


BASELINE = Path(__file__).resolve().parents[1] / "hosted-aws" / "baseline"


def resource_block(source, resource_type, name):
    start = source.index(f'resource "{resource_type}" "{name}" {{')
    depth = 0
    for index in range(start, len(source)):
        if source[index] == "{":
            depth += 1
        elif source[index] == "}":
            depth -= 1
            if depth == 0:
                return source[start : index + 1]
    raise AssertionError(f"Unterminated {resource_type}.{name}")


class HostedZoneSeparationTests(unittest.TestCase):
    def setUp(self):
        self.source = "\n".join(
            path.read_text(encoding="utf-8") for path in sorted(BASELINE.glob("*.tf"))
        )

    def test_cluster_token_scoped_to_hosted_zone_only(self):
        token = resource_block(self.source, "cloudflare_account_token", "cluster_dns")
        zones = re.findall(r"com\.cloudflare\.api\.account\.zone\.\$\{([^}]+)\}", token)
        self.assertEqual(
            zones,
            ["var.hosted_zone_id"],
            "The token every customer cluster holds must cover the hosted zone only.",
        )
        self.assertNotIn("cloudflare_zone_id", token)
        self.assertNotIn("account.zone.*", token)

    def test_hosted_resources_never_use_mail_zone(self):
        hosted = (BASELINE / "hosted_zone.tf").read_text(encoding="utf-8")
        self.assertNotIn(
            "var.cloudflare_zone_id",
            hosted,
            "Customer DNS, origin pulls and the cluster token belong to the hosted zone.",
        )

    def test_contract_publishes_hosted_zone(self):
        contract = (BASELINE / "contract.tf").read_text(encoding="utf-8")
        self.assertRegex(contract, r'"cloudflare-zone-id"\s*=\s*var\.hosted_zone_id')


if __name__ == "__main__":
    unittest.main()
