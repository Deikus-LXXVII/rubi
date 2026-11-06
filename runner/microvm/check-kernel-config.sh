#!/bin/sh
# check-kernel-config.sh <x86_64|aarch64> <.config>: every option the fragments set is in the final configuration, and
# every one they turn off is off. merge_config.sh only warns when an option is dropped (a missing dependency); this
# makes that a failure.
set -eu
arch=$1 config=$2
here=$(cd "$(dirname "$0")" && pwd)
bad=0
for frag in "$here/kernel/common.config" "$here/kernel/$arch.config"; do
	while IFS= read -r line; do
		case $line in
		CONFIG_*=*)
			grep -qx "$line" "$config" || { echo "missing: $line (from ${frag##*/})"; bad=1; }
			;;
		"# CONFIG_"*" is not set")
			opt=${line#\# }; opt=${opt%% *}
			if grep -q "^$opt=" "$config"; then echo "should be off: $opt (from ${frag##*/})"; bad=1; fi
			;;
		esac
	done < "$frag"
done
exit $bad
