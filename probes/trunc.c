#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <arpa/inet.h>
#include <poll.h>
#include <sys/socket.h>
#include <unistd.h>
int main(void) {
  struct sockaddr_in a = {.sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_LOOPBACK)};
  socklen_t l = sizeof a; int on = 1, t = socket(AF_INET, SOCK_DGRAM, 0);
  if (bind(t, (void *)&a, l) || getsockname(t, (void *)&a, &l)) { perror("closed port"); return 2; }
  close(t);
  int s = socket(AF_INET, SOCK_DGRAM, 0);
  if (setsockopt(s, SOL_IP, IP_RECVERR, &on, sizeof on) || sendto(s, "hello", 5, 0, (void *)&a, l) != 5) { perror("setup"); return 2; }
  struct pollfd p = {s, 0, 0}; poll(&p, 1, 1000);
  char d[1], c[20]; struct iovec v = {d, sizeof d};   /* cmsg needs 16 hdr + 16 ee + 16 offender */
  struct msghdr m = {.msg_iov = &v, .msg_iovlen = 1, .msg_control = c, .msg_controllen = sizeof c};
  ssize_t n = recvmsg(s, &m, MSG_ERRQUEUE | MSG_DONTWAIT);
  ssize_t n2 = recv(s, c, sizeof c, MSG_ERRQUEUE | MSG_DONTWAIT); int e2 = errno;
  int ok = n >= 0 && (m.msg_flags & MSG_TRUNC) && (m.msg_flags & MSG_CTRUNC) && n2 < 0 && e2 == EAGAIN;
  printf("%s: n=%zd MSG_TRUNC=%d MSG_CTRUNC=%d controllen_out=%zu; second read n=%zd errno=%d (entry consumed=%d)\n", ok ? "PASS" : "FAIL",
         n, !!(m.msg_flags & MSG_TRUNC), !!(m.msg_flags & MSG_CTRUNC), (size_t)m.msg_controllen, n2, e2, n2 < 0 && e2 == EAGAIN);
  return !ok;
}
