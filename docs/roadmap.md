# Roadmap

This is a hobby project. Keep the next step small and driven by another real
install or a focused test, not by speculative infrastructure.

## Near term

- Repeat the proven 6.5.30 install from a fresh capture and keep the trace and
  serial log with the test result.
- Turn the captured install facts into a small synthetic regression fixture.
- Add explicit package-selection data only when a second install demonstrates a
  different selection.

## Possible future configurations

- A second 6.5.30 media arrangement can be documented after it is proven by a
  separate install.
- MIPSpro/compiler media can be added after the base installation is
  repeatable.
- An older 6.5.22 installation needs matching media and its own boot
  validation.

## Deferred transport and tooling

- NFS remains implementation code with direct tests, but it is not linked into
  the supported server binary.
- A VFS browser or interactive shell is not part of the install path. Add one
  only if a real debugging task needs it.
- Performance work should start with the existing capture timings and a
  reproducible media fixture. Do not add tracing systems before a measurement
  shows a bottleneck.
- 32-bit boot artifacts and other hardware-specific arrangements need their own
  media and hardware evidence.
