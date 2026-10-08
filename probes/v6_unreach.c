#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <arpa/inet.h>
#include <linux/errqueue.h>
#include <poll.h>
#include <sys/socket.h>
#include <unistd.h>
int main(void) {
  struct sockaddr_in6 a = {.sin6_family = AF_INET6, .sin6_addr = IN6ADDR_LOOPBACK_INIT};
  socklen_t l = sizeof a; int on = 1, t = socket(AF_INET6, SOCK_DGRAM, 0);
  if (bind(t, (void *)&a, l) || getsockname(t, (void *)&a, &l)) { perror("closed port"); return 2; }
  close(t);
  int s = socket(AF_INET6, SOCK_DGRAM, 0);
  if (setsockopt(s, SOL_IPV6, IPV6_RECVERR, &on, sizeof on) || sendto(s, "hi", 2, 0, (void *)&a, l) != 2) { perror("setup"); return 2; }
  struct pollfd p = {s, 0, 0}; poll(&p, 1, 1000);
  char d[64], c[256]; struct iovec v = {d, sizeof d};
  struct msghdr m = {.msg_iov = &v, .msg_iovlen = 1, .msg_control = c, .msg_controllen = sizeof c};
  ssize_t n = recvmsg(s, &m, MSG_ERRQUEUE | MSG_DONTWAIT);
  struct cmsghdr *h = n < 0 ? NULL : CMSG_FIRSTHDR(&m);
  if (!h || h->cmsg_level != SOL_IPV6 || h->cmsg_type != IPV6_RECVERR) { printf("FAIL: no IPV6_RECVERR cmsg (n=%zd)\n", n); return 1; }
  struct sock_extended_err *e = (void *)CMSG_DATA(h);
  struct sockaddr_in6 *o = (void *)SO_EE_OFFENDER(e); char ip[64];
  inet_ntop(AF_INET6, &o->sin6_addr, ip, sizeof ip);
  int ok = e->ee_origin == SO_EE_ORIGIN_ICMP6 && e->ee_type == 1 && e->ee_code == 4 && e->ee_errno == ECONNREFUSED;
  printf("%s: errno=%u origin=%u type=%u code=%u offender=%s family=%u\n", ok ? "PASS" : "FAIL",
         e->ee_errno, e->ee_origin, e->ee_type, e->ee_code, ip, o->sin6_family);
  return !ok;
}
