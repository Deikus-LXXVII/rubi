# The release feed (rubi-panel.com/releases/latest.json) from GitHub's list of releases: the newest published
# release. `mac_app` names the Rubi Home app zip, and only when that release really has it, so the panel never
# links to a file that is not there. Run by .github/workflows/pages.yml: gh api ... | jq -f deploy/pages/latest.jq
[.[] | select(.draft == false)] | sort_by(.published_at) | last
| . as $r
| ("rubi-home-" + $r.tag_name + "-macos-universal.zip") as $app
| {version: $r.tag_name, published_at: $r.published_at, notes_url: $r.html_url}
  + (if any($r.assets[]?; .name == $app) then {mac_app: $app} else {} end)
