# Make one file say hello. See ../../README.md.
def run_node_task(inputs):
    if not inputs["path"].startswith("/"):
        return {"status": "failed", "reason": "path must be absolute"}
    greeting = "Hello, " + inputs["person"] + "!\n"
    written = write_file(inputs["path"], greeting)
    if written.error:
        return {"status": "failed", "reason": "MMX could not write the greeting: " + written.error}
    return {"outputs": {"greeting": greeting}}
