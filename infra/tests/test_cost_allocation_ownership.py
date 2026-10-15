"""DES-HOR-603-09: hosted stacks tag resources, but do not activate Billing keys.

Source guards only: no Terraform backend, credentials or cloud API calls. Live
activation must still be verified separately in the Organization management
account. Inspect only root configuration, never ignored state/private inputs.
"""

from pathlib import Path
import unittest


HOSTED_AWS = Path(__file__).resolve().parents[1] / "hosted-aws"


class CostAllocationOwnershipTests(unittest.TestCase):
    def assert_absent_from_hosted_stacks(self, symbol):
        for stack in ("bootstrap", "baseline", "central"):
            directory = HOSTED_AWS / stack
            files = sorted(directory.glob("*.tf")) + sorted(directory.glob("*.tf.json"))
            self.assertTrue(files, f"No Terraform configuration found in {directory}")
            for path in files:
                with self.subTest(stack=stack, file=path.name):
                    self.assertNotIn(
                        symbol,
                        path.read_text(encoding="utf-8"),
                        "Billing activation belongs to the Organization management "
                        "account, not hosted Terraform (DES-HOR-603-09).",
                    )

    def test_no_billing_activation_resource(self):
        self.assert_absent_from_hosted_stacks("aws_ce_cost_allocation_tag")

    def test_no_hosted_activation_toggle(self):
        self.assert_absent_from_hosted_stacks("activate_cost_allocation_tags")


if __name__ == "__main__":
    unittest.main()
