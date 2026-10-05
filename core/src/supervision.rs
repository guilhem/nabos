//! systemd owns recovery when the GPIO workers cannot turn their motors off.
//! Readiness is acknowledged before calibration may energize any output.

use std::io;
use std::mem;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::os::linux::net::SocketAddrExt;
use std::os::unix::net::{SocketAddr, UnixDatagram};
use std::time::{Duration, Instant};

pub struct Watchdog {
    socket: UnixDatagram,
    pub interval: Duration,
}

impl Watchdog {
    pub fn from_env(simulated: bool) -> Result<Option<Self>, String> {
        // Private-bus simulation must never notify the host's service manager.
        if simulated {
            return Ok(None);
        }
        if let Ok(pid) = std::env::var("WATCHDOG_PID") {
            if pid.parse::<u32>().ok() != Some(std::process::id()) {
                return Err("systemd watchdog belongs to another process".into());
            }
        }
        let period = std::env::var("WATCHDOG_USEC")
            .ok()
            .and_then(|v| v.parse::<u64>().ok())
            .filter(|v| *v >= 4)
            .ok_or("physical ears require an active systemd watchdog")?;
        let address = std::env::var("NOTIFY_SOCKET")
            .map_err(|_| "physical ears require systemd's notification socket")?;
        Self::connect(&address, Duration::from_micros(period / 4))
            .map(Some)
            .map_err(|e| format!("systemd notification socket: {e}"))
    }

    fn connect(address: &str, interval: Duration) -> io::Result<Self> {
        let address = if let Some(name) = address.strip_prefix('@') {
            SocketAddr::from_abstract_name(name.as_bytes())?
        } else if address.starts_with('/') {
            SocketAddr::from_pathname(address)?
        } else {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "invalid notify socket",
            ));
        };
        let socket = UnixDatagram::unbound()?;
        socket.set_write_timeout(Some(Duration::from_millis(100)))?;
        socket.connect_addr(&address)?;
        Ok(Self {
            socket,
            interval: interval.min(Duration::from_millis(250)),
        })
    }

    pub fn ready(&self) -> Result<(), String> {
        self.socket
            .send(b"READY=1\nWATCHDOG=1")
            .map_err(|e| e.to_string())?;
        self.barrier(Duration::from_millis(500))
            .map_err(|e| format!("systemd readiness was not acknowledged: {e}"))
    }

    pub fn ping(&self, workers_healthy: bool) -> Result<(), String> {
        if !workers_healthy {
            return Err("ear worker stopped checking motor safety".into());
        }
        self.socket
            .send(b"WATCHDOG=1")
            .map(|_| ())
            .map_err(|e| format!("systemd watchdog notification: {e}"))
    }

    fn barrier(&self, timeout: Duration) -> io::Result<()> {
        let mut pipe = [-1; 2];
        // SAFETY: pipe has space for both descriptors; successful descriptors
        // immediately enter OwnedFd so every failure path closes them.
        if unsafe { libc::pipe2(pipe.as_mut_ptr(), libc::O_CLOEXEC) } != 0 {
            return Err(io::Error::last_os_error());
        }
        let reader = unsafe { OwnedFd::from_raw_fd(pipe[0]) };
        let writer = unsafe { OwnedFd::from_raw_fd(pipe[1]) };
        self.send_barrier(writer.as_raw_fd())?;
        drop(writer);
        let deadline = Instant::now() + timeout;
        loop {
            let mut pfd = libc::pollfd {
                fd: reader.as_raw_fd(),
                events: 0,
                revents: 0,
            };
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Err(io::ErrorKind::TimedOut.into());
            }
            let millis = remaining
                .as_millis()
                .saturating_add(1)
                .min(i32::MAX as u128);
            // SAFETY: pfd is a live one-element pollfd array for this call.
            let result = unsafe { libc::poll(&mut pfd, 1, millis as i32) };
            if result < 0 {
                let error = io::Error::last_os_error();
                if error.kind() == io::ErrorKind::Interrupted {
                    continue;
                }
                return Err(error);
            }
            if pfd.revents & libc::POLLHUP != 0 {
                return Ok(());
            }
            if result == 0 {
                return Err(io::ErrorKind::TimedOut.into());
            }
            return Err(io::Error::other("invalid systemd barrier acknowledgement"));
        }
    }

    fn send_barrier(&self, fd: RawFd) -> io::Result<()> {
        let bytes = b"BARRIER=1";
        let mut iov = libc::iovec {
            iov_base: bytes.as_ptr().cast_mut().cast(),
            iov_len: bytes.len(),
        };
        // usize alignment accommodates cmsghdr on both ARMv6 and ARM64.
        let mut control = [0usize; 4];
        // SAFETY: msg and its buffers are live until sendmsg returns. CMSG_SPACE
        // fits the aligned buffer on 32- and 64-bit Linux, with exactly one fd.
        unsafe {
            let mut msg: libc::msghdr = mem::zeroed();
            msg.msg_iov = &mut iov;
            msg.msg_iovlen = 1;
            msg.msg_control = control.as_mut_ptr().cast();
            msg.msg_controllen = libc::CMSG_SPACE(mem::size_of::<RawFd>() as u32) as usize;
            let cmsg = libc::CMSG_FIRSTHDR(&msg);
            (*cmsg).cmsg_level = libc::SOL_SOCKET;
            (*cmsg).cmsg_type = libc::SCM_RIGHTS;
            (*cmsg).cmsg_len = libc::CMSG_LEN(mem::size_of::<RawFd>() as u32) as usize;
            libc::CMSG_DATA(cmsg).cast::<RawFd>().write(fd);
            if libc::sendmsg(self.socket.as_raw_fd(), &msg, libc::MSG_NOSIGNAL) < 0 {
                return Err(io::Error::last_os_error());
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        atomic::{AtomicUsize, Ordering},
        mpsc,
    };

    static NEXT_SOCKET: AtomicUsize = AtomicUsize::new(0);

    fn sockets() -> (Watchdog, UnixDatagram) {
        let name = format!(
            "nabos-watchdog-{}-{}",
            std::process::id(),
            NEXT_SOCKET.fetch_add(1, Ordering::Relaxed)
        );
        let address = SocketAddr::from_abstract_name(name.as_bytes()).unwrap();
        let server = UnixDatagram::bind_addr(&address).unwrap();
        server
            .set_read_timeout(Some(Duration::from_secs(1)))
            .unwrap();
        (
            Watchdog::connect(&format!("@{name}"), Duration::from_millis(250)).unwrap(),
            server,
        )
    }

    fn receive_barrier(server: &UnixDatagram) -> OwnedFd {
        let mut bytes = [0u8; 64];
        let mut control = [0usize; 4];
        let mut iov = libc::iovec {
            iov_base: bytes.as_mut_ptr().cast(),
            iov_len: bytes.len(),
        };
        // SAFETY: recvmsg writes into the provided live payload/control buffers;
        // the received descriptor is owned by the returned OwnedFd.
        unsafe {
            let mut msg: libc::msghdr = mem::zeroed();
            msg.msg_iov = &mut iov;
            msg.msg_iovlen = 1;
            msg.msg_control = control.as_mut_ptr().cast();
            msg.msg_controllen = mem::size_of_val(&control);
            let len = libc::recvmsg(server.as_raw_fd(), &mut msg, libc::MSG_CMSG_CLOEXEC);
            assert_eq!(len, 9);
            assert_eq!(&bytes[..9], b"BARRIER=1");
            let cmsg = libc::CMSG_FIRSTHDR(&msg);
            assert!(!cmsg.is_null());
            assert_eq!((*cmsg).cmsg_level, libc::SOL_SOCKET);
            assert_eq!((*cmsg).cmsg_type, libc::SCM_RIGHTS);
            OwnedFd::from_raw_fd(libc::CMSG_DATA(cmsg).cast::<RawFd>().read())
        }
    }

    #[test]
    fn readiness_waits_for_manager_and_only_healthy_workers_ping() {
        let (watchdog, server) = sockets();
        let (done, result) = mpsc::channel();
        let thread = std::thread::spawn(move || {
            done.send(watchdog.ready()).unwrap();
            watchdog
        });
        let mut bytes = [0; 64];
        let n = server.recv(&mut bytes).unwrap();
        assert_eq!(&bytes[..n], b"READY=1\nWATCHDOG=1");
        let fd = receive_barrier(&server);
        assert!(result.recv_timeout(Duration::from_millis(20)).is_err());
        drop(fd);
        result
            .recv_timeout(Duration::from_secs(1))
            .unwrap()
            .unwrap();
        let watchdog = thread.join().unwrap();
        watchdog.ping(true).unwrap();
        let n = server.recv(&mut bytes).unwrap();
        assert_eq!(&bytes[..n], b"WATCHDOG=1");
        assert!(watchdog.ping(false).is_err());
        server
            .set_read_timeout(Some(Duration::from_millis(20)))
            .unwrap();
        assert!(server.recv(&mut bytes).is_err());
    }

    #[test]
    fn missing_readiness_acknowledgement_fails_closed() {
        let (watchdog, server) = sockets();
        let thread = std::thread::spawn(move || watchdog.ready());
        server.recv(&mut [0; 64]).unwrap();
        let _held = receive_barrier(&server);
        assert!(thread.join().unwrap().is_err());
        assert!(Watchdog::connect("relative", Duration::from_millis(250)).is_err());
        assert!(Watchdog::from_env(true).unwrap().is_none());
    }
}
