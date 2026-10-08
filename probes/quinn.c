#define _GNU_SOURCE
#include <errno.h>
#include <stdio.h>
#include <arpa/inet.h>
#include <poll.h>
#include <sys/socket.h>
#include <unistd.h>
/* Build with -DRECVERR=0/1 -DNERR=<ICMP errors to queue> -DNDRAIN=<errqueue reads, -1 = until EAGAIN> -DEXPECT=<pass condition>. */
int main(void) {
  struct sockaddr_in r = {.sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_LOOPBACK)}, c = r;
  socklen_t l = sizeof r; int on = 1, rs = socket(AF_INET, SOCK_DGRAM, 0), t = socket(AF_INET, SOCK_DGRAM, 0);
  if (bind(rs, (void *)&r, l) || getsockname(rs, (void *)&r, &l) || bind(t, (void *)&c, l) || getsockname(t, (void *)&c, &l)) { perror("bind"); return 2; }
  close(t);
  int s = socket(AF_INET, SOCK_DGRAM, 0);
  if (RECVERR && setsockopt(s, SOL_IP, IP_RECVERR, &on, sizeof on)) { perror("IP_RECVERR"); return 2; }
  for (int i = 0; i < NERR; i++) { while (sendto(s, "x", 1, 0, (void *)&c, l) < 0) if (errno != ECONNREFUSED) { perror("send"); return 2; } usleep(50000); }
  char b[64]; int drained = 0;
  for (int i = 0; NDRAIN < 0 || i < NDRAIN; i++) if (recv(s, b, sizeof b, MSG_ERRQUEUE | MSG_DONTWAIT) < 0) break; else drained++;
  int fails = 0, first = 0;
  while (fails < 5 && sendto(s, "ok", 2, 0, (void *)&r, l) < 0) { if (!fails) first = errno; fails++; }
  usleep(50000); int del = 0;
  while (recv(rs, b, sizeof b, MSG_DONTWAIT) == 2) del++;
  int ok = EXPECT;
  printf("%s: recverr=%d queued=%d drained=%d failed_sends=%d first_errno=%d(%s) delivered=%d\n", ok ? "PASS" : "FAIL",
         RECVERR, NERR, drained, fails, first, first == ECONNREFUSED ? "ECONNREFUSED" : "-", del);
  return !ok;
}
