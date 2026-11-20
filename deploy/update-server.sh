#!/usr/bin/env bash
set -euo pipefail

# 用法: bash update-server.sh <expected_sha256>
#
# 兼容宝塔「Go 项目」守护进程（/www/server/panel/script/project_daemon.py）：
# 守护进程发现进程退出且 /var/tmp/gopids/<name>.pid 存在时会自动拉起。
# 因此先原子替换二进制、再停旧进程——任何自动重启加载的都是新版本；
# 随后等待守护进程拉起，超时才手动启动，避免双实例抢端口。

expected_sha256="${1:-}"
root="/www/wwwroot/go/codearts2api"
binary_name="codearts2api-linux-amd64"
current="${root}/${binary_name}"
staged="${current}.new"
pid_file="${root}/codearts2api.pid"
log_file="/www/wwwlogs/go/codearts2api.log"
daemon_pid_file="/var/tmp/gopids/codearts2api.pid"

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
if [[ -f "${current}" ]]; then
	cp -p "${current}" "${backup}"
fi
chown www:www "${staged}"
chmod 755 "${staged}"

# 先替换文件再停进程：守护进程/手动重启加载的都已是新版本。
mv "${staged}" "${current}"

start_binary() {
	runuser -u www -- bash -c \
		'cd "$1" || exit; nohup "$2" >> "$3" 2>&1 </dev/null & echo $!' \
		_ "${root}" "${current}" "${log_file}" > "${pid_file}"
	if [[ -f "${daemon_pid_file}" ]]; then
		cp "${pid_file}" "${daemon_pid_file}"
	fi
}

# running_new_pid 输出已加载新二进制的进程 pid（按 /proc/<pid>/exe 校验）。
running_new_pid() {
	local pid exe
	for pid in $(pgrep -f '(^|/)codearts2api-linux-amd64$' || true); do
		exe="$(sha256sum "/proc/${pid}/exe" 2>/dev/null | awk '{print $1}' || true)"
		if [[ "${exe}" == "${actual_sha256}" ]]; then
			printf '%s' "${pid}"
			return 0
		fi
	done
	return 1
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

# 守护进程通常在数秒内拉起；最多等 30s，超时再手动启动。
new_pid=""
for _ in $(seq 1 60); do
	if new_pid="$(running_new_pid)"; then
		break
	fi
	new_pid=""
	sleep 0.5
done
if [[ -z "${new_pid}" ]]; then
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
else
	printf '%s\n' "${new_pid}" > "${pid_file}"
	if [[ -f "${daemon_pid_file}" ]]; then
		printf '%s\n' "${new_pid}" > "${daemon_pid_file}"
	fi
fi

printf 'deployed sha256=%s\n' "${actual_sha256}"
printf 'running pid=%s\n' "${new_pid}"
if [[ -f "${backup}" ]]; then
	printf 'backup=%s\n' "${backup}"
fi
