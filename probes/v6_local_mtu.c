#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <arpa/inet.h>
#include <linux/errqueue.h>
#include <sys/socket.h>
#include <unistd.h>
int main(void) {
  struct sockaddr_in6 a = {.sin6_family = AF_INET6, .sin6_addr = IN6ADDR_LOOPBACK_INIT, .sin6_port = htons(9)};
  int on = 1, mtu = 1280, s = socket(AF_INET6, SOCK_DGRAM, 0); static char big[1400];
  if (setsockopt(s, SOL_IPV6, IPV6_RECVERR, &on, sizeof on) || setsockopt(s, SOL_IPV6, IPV6_DONTFRAG, &on, sizeof on) ||
      connect(s, (void *)&a, sizeof a) || setsockopt(s, SOL_IPV6, IPV6_MTU, &mtu, sizeof mtu)) { perror("setup"); return 2; }
  ssize_t r = send(s, big, sizeof big, 0); int se = errno;
  char d[64], c[256]; struct iovec v = {d, sizeof d};
  struct msghdr m = {.msg_iov = &v, .msg_iovlen = 1, .msg_control = c, .msg_controllen = sizeof c};
  ssize_t n = recvmsg(s, &m, MSG_ERRQUEUE | MSG_DONTWAIT);
  struct cmsghdr *h = n < 0 ? NULL : CMSG_FIRSTHDR(&m);
  if (!h) { printf("FAIL: send=%zd errno=%d, no errqueue entry (n=%zd errno=%d)\n", r, se, n, errno); return 1; }
  struct sock_extended_err *e = (void *)CMSG_DATA(h);
  int ok = r < 0 && se == EMSGSIZE && e->ee_origin == SO_EE_ORIGIN_LOCAL && e->ee_errno == EMSGSIZE && e->ee_info == 1280;
  printf("%s: send=%zd errno=%d; cmsg level=%d type=%d errno=%u origin=%u info=%u offender_family=%u\n", ok ? "PASS" : "FAIL",
         r, se, h->cmsg_level, h->cmsg_type, e->ee_errno, e->ee_origin, e->ee_info, ((struct sockaddr *)SO_EE_OFFENDER(e))->sa_family);
  return !ok;
}
