#!/bin/sh
# UniTAO 镜像的入口：容器以任意非 root UID 运行时（compose 的 user: "${UID}:${GID}"），镜像的 /etc/passwd 里
# 没有这个 UID，ssh 会报 "No user exists for uid" 并退出，git 通道就无法走 SSH。这里在启动前给当前 UID 补一条记录，
# home 为 $HOME（缺省 /tmp）。/etc/passwd 在镜像里是可写的（见 dockerfile）；镜像里没有 setuid 程序，改它不会带来提权。
# 已经有记录（root、或挂进来的 passwd 里已经包含）时什么都不做；/etc/passwd 不可写（例如以只读方式挂了别的文件）时只告警。
uid=$(id -u)
if ! grep -q "^[^:]*:[^:]*:${uid}:" /etc/passwd 2>/dev/null; then
    # 直接尝试写：[ -w ] 只看权限位，看不出只读挂载
    if ! { echo "unitao:x:${uid}:$(id -g):UniTAO:${HOME:-/tmp}:/sbin/nologin" >> /etc/passwd; } 2>/dev/null; then
        echo "entrypoint: uid ${uid} has no /etc/passwd entry and /etc/passwd is not writable; ssh (git channel) will not work" >&2
    fi
fi
# 没有指定命令时与原镜像一样进入 shell
[ $# -eq 0 ] && set -- /bin/sh
exec "$@"
