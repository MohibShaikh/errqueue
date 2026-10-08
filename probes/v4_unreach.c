#define _GNU_SOURCE
#include <stdio.h>
#include <arpa/inet.h>
#include <linux/errqueue.h>
#include <errno.h>
#include <poll.h>
#include <sys/socket.h>
#include <unistd.h>
int main(void) {
  struct sockaddr_in a = {.sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_LOOPBACK)};
  socklen_t l = sizeof a; int on = 1, t = socket(AF_INET, SOCK_DGRAM, 0);
  if (bind(t, (void *)&a, l) || getsockname(t, (void *)&a, &l)) { perror("closed port"); return 2; }
  close(t);
  int s = socket(AF_INET, SOCK_DGRAM, 0);
  if (setsockopt(s, SOL_IP, IP_RECVERR, &on, sizeof on) || sendto(s, "hi", 2, 0, (void *)&a, l) != 2) { perror("setup"); return 2; }
  struct pollfd p = {s, 0, 0}; poll(&p, 1, 1000);
  char d[64], c[256]; struct iovec v = {d, sizeof d};
  struct msghdr m = {.msg_iov = &v, .msg_iovlen = 1, .msg_control = c, .msg_controllen = sizeof c};
  ssize_t n = recvmsg(s, &m, MSG_ERRQUEUE | MSG_DONTWAIT);
  struct cmsghdr *h = n < 0 ? NULL : CMSG_FIRSTHDR(&m);
  if (!h || h->cmsg_level != SOL_IP || h->cmsg_type != IP_RECVERR) { printf("FAIL: no IP_RECVERR cmsg (n=%zd)\n", n); return 1; }
  struct sock_extended_err *e = (void *)CMSG_DATA(h);
  struct sockaddr_in *o = (void *)SO_EE_OFFENDER(e); char ip[32];
  inet_ntop(AF_INET, &o->sin_addr, ip, sizeof ip);
  int ok = e->ee_origin == SO_EE_ORIGIN_ICMP && e->ee_type == 3 && e->ee_code == 3 && e->ee_errno == ECONNREFUSED;
  printf("%s: revents=0x%x errno=%u origin=%u type=%u code=%u offender=%s payload=%zd bytes \"%.*s\"\n", ok ? "PASS" : "FAIL",
         p.revents, e->ee_errno, e->ee_origin, e->ee_type, e->ee_code, ip, n, (int)n, d);
  return !ok;
}
