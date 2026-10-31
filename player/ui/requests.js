// Mutations may have reached the speaker even if the response is lost.
// Bound their lifetime, release UI controls, and never replay them automatically.
async function speakerCommand(path, command) {
 const controller = new AbortController();
 const timer = setTimeout(() => controller.abort(), 8000);
 try {
  const response = await fetch(path, {method: "POST", signal: controller.signal,
   headers: {"Content-Type": "application/json", "X-Slopify-Control": "1"}, body: JSON.stringify(command)});
  if (!response.ok) throw new Error(await response.text());
  return response.status === 204 ? null : await response.json();
 } catch (error) {
  if (controller.signal.aborted) throw new Error("Speaker request timed out. Check the current state before retrying.");
  throw error;
 } finally { clearTimeout(timer); }
}
