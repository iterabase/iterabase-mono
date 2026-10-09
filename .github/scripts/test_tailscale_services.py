import unittest

import tailscale_services as ts


class FakeAPI:
    def __init__(self, services=None, devices=None):
        self.calls = []
        self.services = services or []
        self.devices = devices or []
        self.existing = {}

    def __call__(self, method, path, body=None):
        self.calls.append((method, path, body))
        if method == "GET" and path.endswith("/devices"):
            return {"devices": self.devices}
        if method == "GET" and "/vip-services/" in path:
            return self.existing.get(path.rsplit("/", 1)[1])
        if method == "GET":
            return {"vipServices": self.services}
        if method == "POST":
            return {"key": "tskey-auth-abc-123"}
        return None


class TailscaleServicesTests(unittest.TestCase):
    def test_service_names_cover_both_surfaces(self):
        self.assertEqual(ts.service_names("pr-133"), ["svc:pr-133-app", "svc:pr-133-inference"])
        self.assertEqual(ts.service_names("staging"), ["svc:staging-app", "svc:staging-inference"])
        for bad in ("pr-0", "pr-", "main", "pr-1;x", "PR-1", "pr-12345678"):
            with self.assertRaises(ts.TailscaleError):
                ts.service_names(bad)

    def test_up_creates_tagged_https_services(self):
        fake = FakeAPI()
        ts.services_up(fake, "pr-7")
        puts = [c for c in fake.calls if c[0] == "PUT"]
        self.assertEqual([c[1] for c in puts], [
            "/tailnet/-/vip-services/svc:pr-7-app",
            "/tailnet/-/vip-services/svc:pr-7-inference",
        ])
        for _, _, body in puts:
            self.assertEqual(body["ports"], ["tcp:443"])
            self.assertEqual(body["tags"], [ts.SERVICE_TAG])
            self.assertNotIn("addrs", body, "a new Service gets its addresses from Tailscale")

    def test_up_keeps_existing_service_addresses(self):
        fake = FakeAPI()
        fake.existing["svc:pr-7-app"] = {"name": "svc:pr-7-app", "addrs": ["100.100.1.2", "fd7a:115c:a1e0::1:2"]}
        ts.services_up(fake, "pr-7")
        puts = {c[1]: c[2] for c in fake.calls if c[0] == "PUT"}
        self.assertEqual(puts["/tailnet/-/vip-services/svc:pr-7-app"]["addrs"], ["100.100.1.2", "fd7a:115c:a1e0::1:2"])
        self.assertNotIn("addrs", puts["/tailnet/-/vip-services/svc:pr-7-inference"])

    def test_down_deletes_both(self):
        fake = FakeAPI()
        ts.services_down(fake, "pr-7")
        self.assertEqual([c[0] for c in fake.calls], ["DELETE", "DELETE"])

    def test_prune_deletes_only_orphaned_preview_services(self):
        fake = FakeAPI(services=[
            {"name": "svc:pr-1-app", "tags": [ts.SERVICE_TAG]},          # orphan
            {"name": "svc:pr-2-inference", "tags": [ts.SERVICE_TAG]},    # live
            {"name": "svc:staging-app", "tags": [ts.SERVICE_TAG]},       # live
            {"name": "svc:pr-3-app", "tags": ["tag:other"]},             # not ours
            {"name": "svc:database", "tags": [ts.SERVICE_TAG]},          # not a preview name
        ])
        removed = ts.services_prune(fake, {"pr-2", "staging"})
        self.assertEqual(removed, ["svc:pr-1-app"])
        self.assertEqual([c for c in fake.calls if c[0] == "DELETE"],
                         [("DELETE", "/tailnet/-/vip-services/svc:pr-1-app", None)])

    def test_down_and_prune_remove_preview_nodes_only(self):
        devices = [
            {"id": "1", "hostname": "iterabase-pr-7", "name": "iterabase-pr-7.tail.ts.net", "tags": [ts.HOST_TAG]},
            {"id": "2", "hostname": "iterabase-pr-8", "name": "iterabase-pr-8.tail.ts.net", "tags": [ts.HOST_TAG]},
            {"id": "3", "hostname": "iterabase-staging", "name": "iterabase-staging.tail.ts.net", "tags": [ts.HOST_TAG]},
            {"id": "4", "hostname": "iterabase-pr-9", "name": "laptop", "tags": []},          # untagged: not a preview
            {"id": "5", "hostname": "nuno-laptop", "name": "nuno-laptop", "tags": [ts.HOST_TAG]},
        ]
        fake = FakeAPI(devices=devices)
        self.assertEqual(ts.devices_down(fake, "pr-7"), ["iterabase-pr-7.tail.ts.net"])
        self.assertEqual([c for c in fake.calls if c[0] == "DELETE"], [("DELETE", "/device/1", None)])

        fake = FakeAPI(devices=devices)
        self.assertEqual(ts.devices_prune(fake, {"pr-8", "staging"}), ["iterabase-pr-7.tail.ts.net"])
        self.assertEqual([c[1] for c in fake.calls if c[0] == "DELETE"], ["/device/1"])

    def test_host_key_is_ephemeral_preauthorized_and_tagged(self):
        fake = FakeAPI()
        self.assertEqual(ts.mint_host_key(fake), "tskey-auth-abc-123")
        create = fake.calls[0][2]["capabilities"]["devices"]["create"]
        self.assertEqual(create, {"reusable": False, "ephemeral": True, "preauthorized": True, "tags": [ts.HOST_TAG]})
        self.assertEqual(fake.calls[0][2]["expirySeconds"], 3600)


if __name__ == "__main__":
    unittest.main()
