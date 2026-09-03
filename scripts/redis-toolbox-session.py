#!/usr/bin/env python3
import argparse
import os
import pathlib
import sys
import uuid

import yaml


CONFIG_PATH = pathlib.Path("/etc/redis-toolbox/targets.yaml")
REDIS_CLI_PATH = "/usr/bin/redis-cli"
CA_CERTIFICATE_PATH = "/etc/ssl/certs/ca-certificates.crt"


class ConfigError(ValueError):
    pass


def require_mapping(value, field):
    if not isinstance(value, dict):
        raise ConfigError(f"{field} must be a mapping")
    return value


def require_string(value, field):
    if not isinstance(value, str) or not value.strip():
        raise ConfigError(f"{field} must be a non-empty string")
    return value


def require_boolean(value, field):
    if not isinstance(value, bool):
        raise ConfigError(f"{field} must be a boolean")
    return value


def validate_target(target_id, target):
    require_mapping(target, f"targets.{target_id}")
    forbidden_keys = {"password", "auth_token", "secret", "token"}
    present_forbidden_keys = forbidden_keys.intersection(target)
    if present_forbidden_keys:
        raise ConfigError(f"targets.{target_id} must not contain credentials")

    require_string(target.get("display_name"), f"targets.{target_id}.display_name")
    host = require_string(target.get("host"), f"targets.{target_id}.host")
    if any(character.isspace() for character in host):
        raise ConfigError(f"targets.{target_id}.host must not contain whitespace")

    port = target.get("port")
    if not isinstance(port, int) or isinstance(port, bool) or not 1 <= port <= 65535:
        raise ConfigError(f"targets.{target_id}.port must be a valid TCP port")

    require_boolean(target.get("tls"), f"targets.{target_id}.tls")
    require_boolean(target.get("cluster"), f"targets.{target_id}.cluster")

    username = target.get("username")
    if username is not None:
        require_string(username, f"targets.{target_id}.username")
        if any(character.isspace() for character in username):
            raise ConfigError(f"targets.{target_id}.username must not contain whitespace")

    auth = target.get("auth", {"mode": "prompt"})
    require_mapping(auth, f"targets.{target_id}.auth")
    auth_mode = auth.get("mode")
    if auth_mode not in {"prompt", "static-password"}:
        raise ConfigError(f"targets.{target_id}.auth.mode must be prompt or static-password")
    if auth_mode == "static-password":
        require_string(auth.get("password"), f"targets.{target_id}.auth.password")
    elif "password" in auth:
        raise ConfigError(f"targets.{target_id}.auth.password requires static-password mode")


def load_config(path):
    try:
        with path.open(encoding="utf-8") as config_file:
            config = yaml.safe_load(config_file)
    except (OSError, yaml.YAMLError) as error:
        raise ConfigError(f"Unable to load configuration: {error}") from error

    require_mapping(config, "configuration")
    if config.get("version") != 1:
        raise ConfigError("version must be 1")

    profiles = require_mapping(config.get("profiles"), "profiles")
    targets = require_mapping(config.get("targets"), "targets")
    if not profiles:
        raise ConfigError("profiles must not be empty")
    for target_id, target in targets.items():
        require_string(target_id, "targets key")
        validate_target(target_id, target)

    for profile_id, profile in profiles.items():
        require_string(profile_id, "profiles key")
        require_mapping(profile, f"profiles.{profile_id}")
        require_string(profile.get("environment"), f"profiles.{profile_id}.environment")
        require_string(profile.get("permission"), f"profiles.{profile_id}.permission")
        profile_targets_value = profile.get("targets")
        if not isinstance(profile_targets_value, list):
            raise ConfigError(f"profiles.{profile_id}.targets must be a list")
        for target_id in profile_targets_value:
            require_string(target_id, f"profiles.{profile_id}.targets entry")
            if target_id not in targets:
                raise ConfigError(f"profiles.{profile_id} references unknown target {target_id}")

    return config


def profile_targets(config, profile_id):
    profiles = config["profiles"]
    if profile_id not in profiles:
        raise ConfigError("Redis profile is not available")

    profile = profiles[profile_id]
    targets = []
    for target_id in profile["targets"]:
        if target_id not in config["targets"]:
            raise ConfigError(f"Redis profile references unknown target {target_id}")
        target = dict(config["targets"][target_id])
        target["id"] = target_id
        targets.append(target)
    return profile, targets


def build_command(target):
    command = [
        REDIS_CLI_PATH,
        "-h",
        target["host"],
        "-p",
        str(target["port"]),
    ]
    if target["tls"]:
        command.extend(["--tls", "--cacert", CA_CERTIFICATE_PATH])
    if target["cluster"]:
        command.append("-c")
    if target.get("username"):
        command.extend(["--user", target["username"]])
    if target.get("auth", {"mode": "prompt"})["mode"] == "prompt":
        command.append("--askpass")
    return command


def build_environment(target):
    environment = {
        "HOME": "/home/redis-toolbox",
        "LANG": "C.UTF-8",
        "PATH": "/usr/bin:/bin",
        "REDISCLI_HISTFILE": "/dev/null",
        "REDISCLI_RCFILE": "/dev/null",
        "TERM": "xterm-256color",
        "USER": "redis-toolbox",
    }
    auth = target.get("auth", {"mode": "prompt"})
    if auth["mode"] == "static-password":
        environment["REDISCLI_AUTH"] = auth["password"]
    return environment


def select_target(profile, targets):
    print(f"Environment: {profile['environment']}")
    print(f"Permission:  {profile['permission']}")
    print()
    if not targets:
        raise ConfigError("No Redis targets are configured for this profile")
    for index, target in enumerate(targets, start=1):
        print(f"[{index}] {target['display_name']}")

    while True:
        try:
            selected = input("\nSelect target: ").strip()
        except EOFError as error:
            raise ConfigError("Interactive terminal is required") from error
        if selected.isdigit() and 1 <= int(selected) <= len(targets):
            return targets[int(selected) - 1]
        print(f"Select a number between 1 and {len(targets)}.", file=sys.stderr)


def start_session(target, permission):
    print()
    print(f"Session ID: {uuid.uuid4()}")
    print(f"Target: {target['display_name']}")
    print(f"Mode: {permission}")
    print()

    command = build_command(target)
    os.execve(command[0], command, build_environment(target))


def parse_args():
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--config", type=pathlib.Path, default=CONFIG_PATH)
    parser.add_argument("--profile")
    parser.add_argument("--validate-config", action="store_true")
    return parser.parse_args()


def main():
    arguments = parse_args()
    try:
        config = load_config(arguments.config)
        if arguments.validate_config:
            print("Configuration is valid")
            return 0
        if not arguments.profile:
            raise ConfigError("--profile is required")
        profile, targets = profile_targets(config, arguments.profile)
        target = select_target(profile, targets)
        start_session(target, profile["permission"])
        return 0
    except ConfigError as error:
        print(str(error), file=sys.stderr)
        return 64
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())