//! Compatibility for ureq 3.4.2 timed reads on Linux: a signal can return EINTR
//! even with SA_RESTART. Resume the same read, never resend an HTTP request.

use std::io;
use std::time::Instant;

use ureq::unversioned::transport::{Buffers, ConnectionDetails, Connector, NextTimeout, Transport, time::Duration};

#[derive(Debug)]
pub(super) struct InterruptedConnector;

#[derive(Debug)]
pub(super) struct InterruptedTransport<T>(T);

impl<T: Transport> Connector<T> for InterruptedConnector {
    type Out = InterruptedTransport<T>;

    fn connect(&self, _: &ConnectionDetails, chained: Option<T>) -> Result<Option<Self::Out>, ureq::Error> {
        Ok(chained.map(InterruptedTransport))
    }
}

impl<T: Transport> Transport for InterruptedTransport<T> {
    fn buffers(&mut self) -> &mut dyn Buffers { self.0.buffers() }

    fn transmit_output(&mut self, amount: usize, timeout: NextTimeout) -> Result<(), ureq::Error> {
        // A failed write may have reached the server. Never retry a transmission.
        self.0.transmit_output(amount, timeout)
    }

    fn await_input(&mut self, timeout: NextTimeout) -> Result<bool, ureq::Error> {
        let Duration::Exact(budget) = timeout.after else {
            return self.0.await_input(timeout);
        };
        let started = Instant::now();
        loop {
            let left = budget.saturating_sub(started.elapsed());
            if left.is_zero() {
                return Err(ureq::Error::Timeout(timeout.reason));
            }
            // Restarting a socket read with the original duration would extend
            // its deadline on every signal. Partial headers/body stay buffered.
            let remaining = NextTimeout { after: left.into(), reason: timeout.reason };
            match self.0.await_input(remaining) {
                Err(ureq::Error::Io(error)) if error.kind() == io::ErrorKind::Interrupted => continue,
                result => return result,
            }
        }
    }

    fn is_open(&mut self) -> bool { self.0.is_open() }
    fn is_tls(&self) -> bool { self.0.is_tls() }
}
