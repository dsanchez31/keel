import { openStream, type StreamStatus, type TypedFrame } from '@keel/sdk';
import { useEffect, useEffectEvent, useState } from 'react';

/**
 * keeld's stream for as long as the component is mounted: the SDK's client
 * (resynchronising on a lost frame, redialling a dropped connection), its
 * frames handed to `onFrame`, its status returned.
 *
 * `onFrame` may change between renders without reopening the connection: it
 * is read through an effect event, so the stream always calls the latest one.
 * A `mission` frame with `seq` 1 starts a connection, and whatever `onFrame`
 * built from the previous one is superseded by it.
 */
export function useMissionStream(onFrame: (frame: TypedFrame) => void): StreamStatus {
  const [status, setStatus] = useState<StreamStatus>('connecting');
  const handleFrame = useEffectEvent(onFrame);

  useEffect(() => {
    const stream = openStream({ onFrame: (frame) => handleFrame(frame), onStatus: setStatus });
    return () => stream.close();
  }, []);

  return status;
}
