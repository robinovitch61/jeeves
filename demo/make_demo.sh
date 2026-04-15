#!/usr/bin/env sh

_THISDIR=$(dirname "$(readlink -f "$0")")

# demo.tape output depends on the current working directory
cd "$_THISDIR" || exit

vhs "$_THISDIR"/demo.tape && open "$_THISDIR"/demo.gif

cd - || return
