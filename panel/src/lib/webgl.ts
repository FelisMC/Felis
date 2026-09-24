// webglAvailable probes once whether this browser can create a WebGL context
// (hardware acceleration off, some VMs and remote desktops cannot). The probe
// context is released right away so the fleet view still gets its own.

let cached: boolean | undefined;

export function webglAvailable(): boolean {
  if (cached !== undefined) return cached;
  try {
    const canvas = document.createElement("canvas");
    const gl = canvas.getContext("webgl2") ?? canvas.getContext("webgl");
    gl?.getExtension("WEBGL_lose_context")?.loseContext();
    cached = gl != null;
  } catch {
    cached = false;
  }
  return cached;
}
