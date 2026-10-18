// Release the probe WebGL context immediately so the fleet renderer can create its own.

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
