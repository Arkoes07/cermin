# AGPL-3.0 license, no outside contributions until the business gate

Cermin is open source so it can serve as a readable portfolio piece, but whether it becomes a business is still undecided ([ADR-0002](0002-single-user-business-deferred.md)). We chose AGPL-3.0 over MIT or Apache because relicensing only works cleanly in one direction. As the sole copyright holder, I can relax AGPL to MIT later at no cost. Anything released under MIT, though, stays MIT permanently, and anyone could run it as a closed competing service. Outside pull requests are not accepted until the business gate is decided, so the copyright stays with one holder and relicensing doesn't need a contributor agreement (CLA).

## Considered Options

- **MIT / Apache-2.0**: friendlier if companies want to adopt the MCP server, but the choice can't be taken back for any version already released.
- **FSL / BSL (source-available)**: blocks competing commercial use outright and later converts to an open license, but isn't OSI-approved open source and is less familiar to readers.
