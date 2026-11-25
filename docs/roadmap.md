# Roadmap

This is a hobby project. New work should answer a problem found during an
install or a focused test.

## Installation coverage

- Repeat the Octane2 IRIX 6.5.30 install with a fresh capture and serial log.
- Test the IRIX 6.5.22 config and 32-bit ARCS boot path on an Indy or Indigo2.
- Install IRIX 5.3 and 6.2 on real hardware.
- Test the R3000 Indigo, which boots `fx.IP12` from the IRIX 5.3 disc.
- Verify extra package selections, such as `dbx`, and a maintenance-stream
  install end to end. Keep the guest output with each result.
- Validate the shipped MIPSpro add-on on the target configurations. Having a
  media config is separate from a completed compiler installation.
- Turn captured install behaviour into small synthetic regression fixtures.

## File sharing and networking

- Observe DHCP lease renewal and rebinding from IRIX, which the one-year
  lease has kept out of every run so far. See [Addressing the
  machine](network-reference.md#addressing-the-machine-bootp-dhcp-and-the-guest)
  in the network reference.

## Further work

Performance changes should start with capture timings and reproducible media.
The [QEMU measurements](qemu-network-performance.md) document one comparison
and its limits. An interactive media browser can wait until a debugging task
needs one.
