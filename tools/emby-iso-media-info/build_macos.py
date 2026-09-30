#!/usr/bin/env python3
"""Build with official Roslyn 4.8 nupkg and the installed Emby .NET runtime."""
import argparse, ctypes, json, pathlib, tempfile, zipfile
parser = argparse.ArgumentParser()
parser.add_argument("compiler_package", type=pathlib.Path)
parser.add_argument("--emby", type=pathlib.Path, default=pathlib.Path("/Applications/EmbyServer.app/Contents/MacOS"))
parser.add_argument("--output", type=pathlib.Path, default=pathlib.Path("STRMhub.IsoMediaInfo.dll"))
args = parser.parse_args()
with tempfile.TemporaryDirectory(prefix="strmhub-iso-compiler-") as directory:
    root = pathlib.Path(directory)
    with zipfile.ZipFile(args.compiler_package) as package:
        prefix = "tasks/netcore/bincore/"
        for name in package.namelist():
            leaf = name.removeprefix(prefix)
            if name.startswith(prefix) and leaf and "/" not in leaf:
                (root / leaf).write_bytes(package.read(name))
    for library in args.emby.iterdir():
        if library.suffix in (".dll", ".dylib") and not (root / library.name).exists():
            (root / library.name).symlink_to(library)
    (root / "csc.runtimeconfig.json").write_bytes((args.emby / "EmbyServer.runtimeconfig.json").read_bytes())
    runtime = json.loads((args.emby / "EmbyServer.deps.json").read_text())
    compiler = json.loads((root / "csc.deps.json").read_text())
    target = runtime["runtimeTarget"]["name"]
    for dependencies in compiler["targets"].values():
        runtime["targets"][target].update(dependencies)
    runtime["libraries"].update(compiler["libraries"])
    (root / "csc.deps.json").write_text(json.dumps(runtime))
    host = ctypes.CDLL(str(root / "libhostfxr.dylib"))
    init = host.hostfxr_initialize_for_dotnet_command_line
    init.argtypes = [ctypes.c_int, ctypes.POINTER(ctypes.c_char_p), ctypes.c_void_p, ctypes.POINTER(ctypes.c_void_p)]
    host.hostfxr_run_app.argtypes = [ctypes.c_void_p]
    host.hostfxr_close.argtypes = [ctypes.c_void_p]
    flags = [str(root / "csc.dll"), "-nologo", "-noconfig", "-nostdlib+", "-target:library", "-out:" + str(args.output.resolve())]
    flags += ["-r:" + str(lib) for lib in args.emby.glob("*.dll")]
    flags += [str(pathlib.Path(__file__).with_name("Plugin.cs").resolve())]
    argv = (ctypes.c_char_p * len(flags))(*[flag.encode() for flag in flags])
    handle = ctypes.c_void_p()
    result = init(len(flags), argv, None, ctypes.byref(handle))
    if result: raise SystemExit("Compiler runtime failed: " + hex(result & 0xffffffff))
    result = host.hostfxr_run_app(handle)
    host.hostfxr_close(handle)
    raise SystemExit(result)
