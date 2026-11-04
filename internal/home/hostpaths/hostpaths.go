// Package hostpaths is where Rubi Home keeps and finds things on this computer, and which folders it never works
// in: one file per system (paths_darwin.go, paths_linux.go, paths_windows.go) with the same functions.
//
//   - DataBase: the folder that holds Home's own folder ("Rubi Home"): the user's configuration folder on macOS and
//     Linux (XDG), %LOCALAPPDATA% on Windows (not %AppData%, which roams and would copy Home's keys).
//   - HelperHome: the folder of the installed copy of Home and its logs (~/.rubi-home).
//   - HookBinary, PluginDir, ManagedSettings: the root- or administrator-owned places Claude Code reads.
//   - The deny lists: folders that are never a project and never a session's working folder.
//
// The values of macOS are those Home always used; tests pin them (docs/plans/home-platforms.md, stage P2).
package hostpaths
