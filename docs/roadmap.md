# Roadmap

This is a hobby project. New work should answer a problem found during an
install or a focused test.

## Installation coverage

- Repeat the Octane2 6.5.30 install with a fresh capture and serial log.
- Test the 6.5.22 config and 32-bit ARCS boot path on an Indy or Indigo2.
  The config and boot-file support are present. Hardware acceptance is pending.
- Verify extra package selections, such as `dbx`, and a maintenance-stream
  install end to end. Keep the guest output with each result.
- Validate the shipped MIPSpro add-on on the target configurations. Having a
  media config is separate from a completed compiler installation.
- Turn captured install behavior into small synthetic regression fixtures.

## File sharing and networking

NFSv2/v3 shares, private-network DHCP, and the shared RPC and Ethernet frame
transcripts are implemented. See the [sharing guide](sharing.md) for usage and
the [protocol reference](nfs-shares.md) for behavior and recorded checks.

The DHCP server identifier is configurable so an installed IRIX guest can
move between private and QEMU user networks. The
[sharing guide](sharing.md#moving-between-networks) explains the setting and
the remaining lease-renewal limitation.

## Further work

Performance changes should start with capture timings and reproducible media.
The [QEMU measurements](qemu-network-performance.md) document one comparison
and its limits. An interactive media browser can wait until a debugging task
needs one.
