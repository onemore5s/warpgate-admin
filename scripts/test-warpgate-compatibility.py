#!/usr/bin/env python3
"""Verify the CLI against real Warpgate containers, operating only on local test resources."""

import argparse
import base64
import csv
import json
import os
from pathlib import Path
import secrets
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid


IMAGE = "ghcr.io/warp-tech/warpgate"
VERSIONS = ("0.28.2", "0.29.0", "0.29.2", "0.30.0")
HTTP_PORT, SSH_PORT = "8888", "2222"
READY_TIMEOUT, COMMAND_TIMEOUT = 60, 180
GROUP = "compat-group-placeholder"
ROLES = ("compat-reader-placeholder", "compat-writer-placeholder")
POLICIES = ("ticket_requests_disabled", "ticket_require_approval", "require_approval")
LIMITS = {"rate_limit_bytes_per_second": 4096, "ticket_max_duration_seconds": 600, "ticket_max_uses": 3}
IDLE_TIMEOUT = "37s"
EXTERNAL_HOST = "gateway.example.invalid"
TARGET_NAMES = ("mysql-placeholder", "postgres-placeholder", "http-placeholder")
CSV_FIELDS = ("name", "group", "engine", "host", "port", "username", "password", "password_env", "tls_mode",
              "tls_verify", "default_database_name", "protocol_version", "roles", "description", "enabled")
OPERATIONS = {
    "/target-groups": ("get", "post"), "/target-groups/{id}": ("put",),
    "/targets": ("get", "post"), "/targets/{id}": ("put", "delete"),
    "/roles": ("get",), "/targets/{id}/roles": ("get",),
    "/targets/{id}/roles/{role_id}": ("post", "delete"),
}
NO_CHANGES = "0 to create, 0 to update, 0 to delete"
REPOSITORY = Path(__file__).resolve().parent.parent


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def changed_paths(before, after, path=""):
    if isinstance(before, dict) and isinstance(after, dict):
        missing = [path + "/" + key for key in before.keys() ^ after.keys()]
        return missing + [changed for key in before.keys() & after.keys()
                          for changed in changed_paths(before[key], after[key], path + "/" + key)]
    return [] if before == after else [path]


class Fixture:
    def __init__(self, root, binary):
        self.root, self.binary = root, binary
        self.prefix = "warpgate-api-test-" + uuid.uuid4().hex[:12]
        self.volume, self.containers = self.prefix + "-data", []
        self.volume_created = False
        self.origin, self.version = "", ""
        self.token, self.password = secrets.token_urlsafe(32), secrets.token_urlsafe(24)
        self.env = dict(os.environ, WARPGATE_ADMIN_TOKEN=self.token, WARPGATE_ADMIN_PASSWORD=self.password,
                        WARPGATE_ENCRYPTION_KEY=base64.b64encode(os.urandom(32)).decode(),
                        COMPAT_DATABASE_PASSWORD=self.password, COMPAT_HTTP_TOKEN=self.password)
        # Use self-signed certificates only on random localhost ports; external addresses are not accepted.
        self.tls = ssl._create_unverified_context()
        self.database, self.http, self.config = [], [], {}
        self.extra_group = None

    def command(self, args):
        result = subprocess.run(args, env=self.env, text=True, capture_output=True, timeout=COMMAND_TIMEOUT)
        detail = ""
        if Path(args[0]) == self.binary:
            detail = result.stderr
            for secret in (self.token, self.password, self.env["COMPAT_DATABASE_PASSWORD"]):
                detail = detail.replace(secret, "[redacted]")
        require(result.returncode == 0, f"Command failed: {Path(args[0]).name} {args[1]}, exit code {result.returncode} {detail}")
        return result.stdout.strip()

    def api(self, path, method="GET", body=None, expected_status=None):
        request = urllib.request.Request(self.origin + "/@warpgate/admin/api" + path, method=method,
            data=None if body is None else json.dumps(body).encode(),
            headers={"X-Warpgate-Token": self.token, "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(request, context=self.tls, timeout=10) as response:
                status, raw = response.status, response.read()
        except urllib.error.HTTPError as error:
            status, raw = error.code, b""
        if expected_status is not None:
            require(status == expected_status, f"{method} {path} returned status {status}, expected {expected_status}")
        else:
            require(200 <= status < 300, f"{method} {path} returned HTTP {status}")
        return json.loads(raw) if raw else None

    def setup(self, version):
        self.command(["docker", "volume", "create", "--label", "purpose=warpgate-api-test", self.volume])
        self.volume_created = True
        name = self.prefix + "-setup"
        self.containers.append(name)
        self.command(["docker", "run", "--rm", "--name", name,
            "--mount", f"type=volume,src={self.volume},dst=/data", "--env", "WARPGATE_ADMIN_PASSWORD",
            IMAGE + ":" + version, "unattended-setup", "--data-path=/data",
            "--http-port=" + HTTP_PORT, "--ssh-port=" + SSH_PORT])
        self.containers.remove(name)

    def start(self, version):
        self.version = version
        name = self.prefix + "-" + version.replace(".", "-")
        self.containers.append(name)
        self.command(["docker", "run", "--detach", "--name", name,
            "--mount", f"type=volume,src={self.volume},dst=/data", "--publish", f"127.0.0.1::{HTTP_PORT}",
            "--env", "WARPGATE_ADMIN_TOKEN", "--env", "WARPGATE_ENCRYPTION_KEY",
            IMAGE + ":" + version, "run", "--enable-admin-token"])
        self.origin = "https://" + self.command(["docker", "port", name, HTTP_PORT + "/tcp"]).splitlines()[0]
        deadline = time.monotonic() + READY_TIMEOUT
        while time.monotonic() < deadline:
            try:
                self.api("/targets")
                return name
            except (RuntimeError, urllib.error.URLError, TimeoutError):
                time.sleep(0.5)
        raise RuntimeError(version + " did not become ready before the timeout")

    def schema(self):
        schema = self.api("/openapi.json")
        for path, methods in OPERATIONS.items():
            for method in methods:
                require(method in schema["paths"].get(path, {}), f"Missing API: {method} {path}")
        required = schema["components"]["schemas"]["TargetDataRequest"]["required"]
        if self.version != VERSIONS[0]:
            require(set(POLICIES) <= set(required), "Required approval fields do not match the new API contract")

    def inventory(self):
        self.database = [dict(name=name, group=GROUP, engine=engine, host="database.example.invalid", port=port,
            username="user-placeholder", password_env="COMPAT_DATABASE_PASSWORD", tls_mode="required", tls_verify="true",
            default_database_name="database-placeholder", protocol_version=protocol, roles=ROLES[0],
            description="Initial target", enabled="true") for name, engine, port, protocol in
            [(TARGET_NAMES[0], "mysql", "3306", ""), (TARGET_NAMES[1], "postgres", "5432", "")]]
        self.http = [dict(name=TARGET_NAMES[2], group=GROUP, url="https://service.example.invalid", roles=[ROLES[0]],
            headers_env={"Authorization": "COMPAT_HTTP_TOKEN"}, external_host=EXTERNAL_HOST, description="Initial target")]
        self.config = {"warpgate": {"host": self.origin, "token": self.token, "insecure_skip_verify": True},
            "inventory": {"groups": "groups.csv", "database_targets": "database.csv", "http_targets": "http.yaml"},
            "apply": {"update_passwords": False, "sync_roles": False, "sync_database_targets": False, "sync_http_targets": False}}
        self.group_description = "Initial group"
        self.group_color = "Primary"

    def save(self):
        self.config["warpgate"]["host"] = self.origin
        with (self.root / "groups.csv").open("w", newline="") as output:
            writer = csv.writer(output)
            writer.writerow(["name", "description", "color", "enabled"])
            writer.writerow([GROUP, self.group_description, self.group_color, "true"])
            if self.extra_group:
                writer.writerow([self.extra_group, "Per-version group creation check", "", "true"])
        with (self.root / "database.csv").open("w", newline="") as output:
            writer = csv.DictWriter(output, fieldnames=CSV_FIELDS)
            writer.writeheader()
            writer.writerows(self.database)
        (self.root / "http.yaml").write_text(json.dumps({"targets": self.http}))
        (self.root / "config.yaml").write_text(json.dumps(self.config))
        for path in self.root.iterdir():
            if path.is_file():
                path.chmod(0o600)

    def cli(self, action):
        self.save()
        output = self.command([str(self.binary), action, "--config", str(self.root / "config.yaml")])
        require(not any(secret in output for secret in (self.token, self.password, self.env["COMPAT_DATABASE_PASSWORD"])),
                "CLI output exposed test credentials")
        return output

    def targets(self):
        return {target["name"]: target for target in self.api("/targets") if target["name"] in TARGET_NAMES}

    def snapshot(self):
        targets = self.targets()
        return {name: {"target": target, "roles": self.api("/targets/" + target["id"] + "/roles")}
                for name, target in targets.items()}

    def unchanged(self):
        before = self.snapshot()
        require(NO_CHANGES in self.cli("plan"), "Repeated plan produced unexpected changes")
        require(NO_CHANGES in self.cli("apply"), "Repeated apply produced unexpected changes")
        require(self.snapshot() == before, "An unchanged run modified server state")

    def update_body(self, target):
        return {key: target[key] for key in ("name", "description", "options", "group_id", *POLICIES, *LIMITS) if key in target}

    def exercise(self):
        self.config["apply"]["sync_roles"] = False
        self.extra_group = "compat-extra-" + self.version + "-placeholder"
        self.schema()
        existing_roles = {role["name"] for role in self.api("/roles")}
        for role in ROLES:
            if role not in existing_roles:
                self.api("/roles", "POST", {"name": role, "description": "Compatibility test"})
        self.cli("validate")
        self.cli("plan")
        self.cli("apply")
        require(len(self.targets()) == len(TARGET_NAMES), "Unexpected target count after creation")
        require(self.targets()[TARGET_NAMES[1]]["options"]["protocol_version"] == "3.2", "Empty protocol version did not use the default")
        require(self.extra_group in {group["name"] for group in self.api("/target-groups")}, "Group creation failed")
        self.unchanged()

        # Reject writes missing any required approval field to verify server-side enforcement.
        target = self.targets()[TARGET_NAMES[0]]
        if self.version != VERSIONS[0]:
            for policy in POLICIES:
                body = self.update_body(target)
                body.pop(policy)
                self.api("/targets/" + target["id"], "PUT", body, expected_status=400)
        if self.version == VERSIONS[-1]:
            for field in ("ticket_max_duration_seconds", "ticket_max_uses"):
                for invalid in (0, -1):
                    body = self.update_body(target)
                    body[field] = invalid
                    self.api("/targets/" + target["id"], "PUT", body, expected_status=400)
            require(self.targets()[target["name"]] == target, "Rejecting invalid Ticket values modified the target")

        for target in self.targets().values():
            body = self.update_body(target)
            body.update({policy: True for policy in POLICIES if policy in target})
            body.update(LIMITS)
            if body["options"]["kind"] == "Postgres":
                body["options"]["idle_timeout"] = IDLE_TIMEOUT
            self.api("/targets/" + target["id"], "PUT", body)
        self.group_description, self.group_color = "Updated group " + self.version, "Success"
        for target in [*self.database, *self.http]:
            target["description"] = "Updated target " + self.version
        self.http[0].pop("headers_env", None)
        self.http[0].pop("external_host", None)
        self.cli("apply")
        for target in self.targets().values():
            require(all(target[key] == value for key, value in LIMITS.items()), "Rate limits or Ticket settings were overwritten")
            require(all(target[key] is True for key in POLICIES if key in target), "Existing approval policies were disabled")
            options = target["options"]
            if options["kind"] == "Http":
                require(options["headers"].get("Authorization") == self.password, "Omitting headers did not preserve the existing values")
                require(options["external_host"] == EXTERNAL_HOST, "Omitting external_host did not preserve the existing value")
            else:
                require(options["auth"]["kind"] == "Password", "Incompatible database auth format")
                if options["kind"] == "Postgres":
                    require(options["idle_timeout"] == IDLE_TIMEOUT, "PostgreSQL idle_timeout was lost")
        group = next(group for group in self.api("/target-groups") if group["name"] == GROUP)
        require(group["description"] == self.group_description and group["color"] == self.group_color, "Group update did not take effect")
        self.unchanged()

        for target in self.database:
            target["roles"] = ";".join(ROLES)
        self.http[0]["roles"] = list(ROLES)
        self.cli("apply")
        for target in self.database:
            target["roles"] = ROLES[0]
        self.http[0]["roles"] = [ROLES[0]]
        self.cli("apply")
        self.check_roles(ROLES)
        self.config["apply"]["sync_roles"] = True
        self.cli("apply")
        self.check_roles(ROLES[:1])
        self.unchanged()

        self.env["COMPAT_DATABASE_PASSWORD"] = secrets.token_urlsafe(24)
        self.config["apply"]["update_passwords"] = True
        require("2 to update" in self.cli("plan"), "Forced password update did not select both database targets")
        self.cli("apply")
        self.config["apply"]["update_passwords"] = False
        self.unchanged()

        self.http[0]["headers"], self.http[0]["external_host"] = {}, ""
        self.database[1]["protocol_version"] = "3.2"
        self.cli("apply")
        http = self.targets()[TARGET_NAMES[2]]["options"]
        require(http["headers"] == {} and http["external_host"] in (None, ""), "Explicitly clearing HTTP configuration failed")
        require(self.targets()[TARGET_NAMES[1]]["options"]["protocol_version"] == "3.2", "Protocol version update failed")
        self.unchanged()

        # Preserve either existing protocol version when the inventory leaves it unspecified.
        for protocol in ("3.0", "3.2"):
            self.database[1]["protocol_version"] = protocol
            self.cli("apply")
            require(self.targets()[TARGET_NAMES[1]]["options"]["protocol_version"] == protocol, "Explicit protocol version update failed")
            self.database[1]["protocol_version"] = ""
            self.unchanged()
            self.database[1]["description"] = "Protocol preservation " + protocol
            self.cli("apply")
            require(self.targets()[TARGET_NAMES[1]]["options"]["protocol_version"] == protocol, "Empty protocol version changed the existing version")
            self.unchanged()

        # Create temporary targets for each protocol, then remove them through CLI synchronization.
        original_database, original_http = self.database[:], self.http[:]
        self.database += [dict(target, name="extra-" + target["name"]) for target in original_database]
        self.http += [dict(target, name="extra-" + target["name"]) for target in original_http]
        self.cli("apply")
        extras = {"extra-" + name for name in TARGET_NAMES}
        require(extras <= {target["name"] for target in self.api("/targets")}, "Additional target creation failed")
        self.database, self.http = original_database, original_http
        self.config["apply"].update(sync_database_targets=True, sync_http_targets=True)
        require("3 to delete" in self.cli("plan"), "Deletion plan did not match expectations")
        self.cli("apply")
        require(not extras & {target["name"] for target in self.api("/targets")}, "Synchronization did not delete the extra targets")
        self.unchanged()

        # Restore values so the next version can verify preservation of omitted fields.
        self.http[0].pop("headers")
        self.http[0].update(headers_env={"Authorization": "COMPAT_HTTP_TOKEN"}, external_host=EXTERNAL_HOST)
        self.cli("apply")
        self.unchanged()
        print(f"PASS {self.version}: 11 client API operations, three Target types, groups, role changes, policy preservation, password updates, deletion, idempotency", flush=True)

    def check_roles(self, expected):
        for target in self.targets().values():
            actual = {role["name"] for role in self.api("/targets/" + target["id"] + "/roles")}
            require(actual == set(expected), "Unexpected role assignments")

    def cleanup(self):
        failures = []
        for name in reversed(self.containers):
            if subprocess.run(["docker", "rm", "--force", name], capture_output=True).returncode:
                failures.append(name)
        if self.volume_created:
            if subprocess.run(["docker", "volume", "rm", self.volume], capture_output=True).returncode:
                failures.append(self.volume)
        require(not failures, "Failed to clean up test resources: " + ", ".join(failures))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--versions", nargs="+", choices=VERSIONS, default=list(VERSIONS))
    parser.add_argument("--binary", type=Path, help="CLI binary to verify; defaults to building the current source")
    args = parser.parse_args()
    require(list(args.versions) == sorted(set(args.versions)), "Versions must be unique and listed in upgrade order")
    with tempfile.TemporaryDirectory(prefix="warpgate-api-test-") as temporary:
        root = Path(temporary)
        binary = args.binary.resolve() if args.binary else root / "warpgate-admin"
        if not args.binary:
            subprocess.run(["go", "build", "-o", str(binary), "./cmd/warpgate-admin"], cwd=REPOSITORY, check=True)
        fixture_root = root / "fixture"
        fixture_root.mkdir(mode=0o700)
        fixture = Fixture(fixture_root, binary)
        try:
            fixture.setup(args.versions[0])
            before = None
            for version in args.versions:
                name = fixture.start(version)
                if before is None:
                    fixture.inventory()
                else:
                    after = fixture.snapshot()
                    # 0.29 adds require_approval with a migration default of false; preserve all other data.
                    for target_name, state in after.items():
                        previous = before[target_name]["target"]
                        if "require_approval" not in previous:
                            require(state["target"].pop("require_approval") is False, "Unexpected migration default for the new approval policy")
                            # 0.29 removes the empty legacy password field; auth.password must remain intact.
                            options = previous["options"]
                            if "password" in options:
                                require(options.pop("password") is None, "Legacy password has not been normalized into auth")
                    differences = changed_paths(before, after)
                    require(not differences, "Upgrade changed existing fields: " + ", ".join(differences))
                    fixture.unchanged()
                    print(f"PASS upgrade to {version}: reused database with target identities, configuration, and roles preserved", flush=True)
                fixture.exercise()
                before = fixture.snapshot()
                fixture.command(["docker", "stop", "--time", "10", name])
        finally:
            fixture.cleanup()


if __name__ == "__main__":
    main()
