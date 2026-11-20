#!/usr/bin/env bash
set -euo pipefail

expected_sha256="${1:-}"
root="/www/wwwroot/go/codearts2api"
binary_name="codearts2api-linux-amd64"
current="${root}/${binary_name}"
staged="${current}.new"
pid_file="${root}/codearts2api.pid"
log_file="/www/wwwlogs/go/codearts2api.log"

if [[ ! -f "${staged}" ]]; then
	printf 'staged binary not found: %s\n' "${staged}" >&2
	exit 1
fi

actual_sha256="$(sha256sum "${staged}" | awk '{print $1}')"
if [[ -n "${expected_sha256}" && "${actual_sha256}" != "${expected_sha256}" ]]; then
	printf 'checksum mismatch: expected %s, got %s\n' "${expected_sha256}" "${actual_sha256}" >&2
	exit 1
fi

stamp="$(date +%Y%m%d-%H%M%S)"
backup="${current}.bak-${stamp}"
cp -p "${current}" "${backup}"
chown www:www "${staged}"
chmod 755 "${staged}"

start_binary() {
	runuser -u www -- bash -c \
		'cd "$1" || exit; nohup "$2" >> "$3" 2>&1 </dev/null & echo $!' \
		_ "${root}" "${current}" "${log_file}" > "${pid_file}"
}

old_pids="$(pgrep -f '(^|/)codearts2api-linux-amd64$' || true)"
if [[ -n "${old_pids}" ]]; then
	kill -TERM ${old_pids}
	for _ in $(seq 1 20); do
		running=""
		for pid in ${old_pids}; do
			if kill -0 "${pid}" 2>/dev/null; then
				running="${running} ${pid}"
			fi
		done
		if [[ -z "${running}" ]]; then
			break
		fi
		sleep 0.5
	done
	if [[ -n "${running:-}" ]]; then
		kill -KILL ${running}
	fi
fi

mv "${staged}" "${current}"
start_binary
sleep 2

new_pid="$(cat "${pid_file}")"
if ! kill -0 "${new_pid}" 2>/dev/null; then
	printf 'new process failed to start; restoring %s\n' "${backup}" >&2
	mv "${backup}" "${current}"
	start_binary
	sleep 2
	exit 1
fi

printf 'deployed sha256=%s\n' "${actual_sha256}"
printf 'running pid=%s\n' "${new_pid}"
printf 'backup=%s\n' "${backup}"
