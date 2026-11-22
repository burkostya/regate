# Regate on NixOS

Import the module and point it at the HCL blocks you want to maintain with
the system configuration.

```nix
{
  imports = [ ./regate/nix/module.nix ];

  services.regate = {
    enable = true;
    staticConfigFile = ./regate-static.hcl;
    environmentFiles = [ "/run/secrets/regate.env" ];
  };
}
```

The module builds the gateway and its management skills. It listens on
loopback by default. The environment file must provide
`REGATE_ADMIN_TOKEN`; it can also supply upstream credentials and queue
keys. Keep credential values in the runtime file and use environment
variable names in HCL. The HCL file becomes part of the Nix store.

The systemd service keeps the SQLite database, drafts, layouts, effects
and tasks in `/var/lib/regate` with directory mode `0700`. Preserve that
directory on hosts that reset `/var` at boot. For an existing instance,
stop its worker before transferring its state to the service. Use the
[SQLite backup and restore procedures](../contracts/sqlite.md) when
recovering from a snapshot.

## Static and dynamic HCL

For example, declare an upstream in `regate-static.hcl` and add the
surface that calls it in the console. A deployment publishes a single
effective HCL revision, so both kinds of blocks can refer to each other.
`bootstrapConfigFile` is optional and seeds the dynamic source on first
startup. An instance with no surfaces can be configured through the
authenticated management endpoints. Console and management MCP edits
use drafts, validation and activation to publish changes to the dynamic
portion of the full source.

To promote a block already managed through Regate, read
`regate_config_get` or authenticated `GET /api/config` and copy the block
from `dynamic_source` into the NixOS file without changing its contents.
Run `nixos-rebuild switch`. With an exact match Regate moves ownership of
that top-level block to NixOS and retains the other dynamic HCL bytes.
Change the block through NixOS in subsequent deployments. Named blocks
are matched by type and label, and the unlabeled `regate` block has a
single identity.

The effective source returned by the management API begins with
`static_prefix`. Leave that prefix intact when activating a draft. Old
drafts and revisions remain readable after deployment. Recreate a draft
from the current revision and reapply any pending changes from an older
draft. Regate rollback can use revisions within the current static
deployment. Change the static file through NixOS to revert static blocks.

A mismatch with an existing dynamic block, a duplicate static block or
a broken reference rejects the entire proposed deployment. The service
keeps serving the previous active revision and reports
`static_config_error` through authenticated status and a journal message.
On first startup, a configuration that fails validation prevents service
startup. Repeated starts with the same static file and dynamic state
reuse the existing revision.

Removing a block from the static file removes it from the effective HCL
when the remaining configuration validates. The module uses an empty
static file when the option is unset. NixOS generation changes preserve
SQLite state. Binaries preceding SQLite schema 8 cannot open an upgraded
database, so retain a verified backup before upgrading across that boundary.

Outside NixOS, use `regate -static-config /path/to/static.hcl -state /path/to/state`
with an optional `-config` seed. Omitting `-static-config` keeps the last
deployed static layer and its ownership rules.
