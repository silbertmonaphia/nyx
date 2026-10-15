import { describe, it, expect, vi } from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ConfirmationCard } from './ConfirmationCard';
import type { ChatStepConfirm } from '../types/chat';

function makeConfirm(
  overrides: Partial<ChatStepConfirm> = {},
): ChatStepConfirm {
  return {
    runId: 'run-1',
    approvalId: 'ap-1',
    tool: 'create_feed',
    args: { title: 'Rust Blog', url: 'https://blog.rust-lang.org/feed.xml' },
    expiresAt: Date.now() / 1000 + 300,
    decision: 'pending',
    ...overrides,
  };
}

// One card per paused mutating call (openspec change agent-feed-tools
// §6.2). The card is the decision affordance; the denied call's result
// renders through the regular agent-step path, not here.

describe('ConfirmationCard', () => {
  it('renders the tool name and args while pending', () => {
    render(
      <ConfirmationCard
        confirm={makeConfirm()}
        onDecide={vi.fn()}
        onExpire={vi.fn()}
      />,
    );

    const card = screen.getByTestId('chat-confirm-card');
    expect(card).toHaveAttribute('data-decision', 'pending');
    expect(card).toHaveAttribute('data-approval-id', 'ap-1');
    expect(screen.getByTestId('chat-confirm-tool')).toHaveTextContent(
      'create_feed',
    );
    const args = screen.getByTestId('chat-confirm-args');
    expect(args).toHaveTextContent('Rust Blog');
    expect(args).toHaveTextContent('https://blog.rust-lang.org/feed.xml');
    expect(screen.getByTestId('chat-confirm-approve')).toBeInTheDocument();
    expect(screen.getByTestId('chat-confirm-deny')).toBeInTheDocument();
  });

  it('calls onDecide("approve") when Approve is clicked', async () => {
    const onDecide = vi.fn().mockResolvedValue(undefined);
    render(
      <ConfirmationCard
        confirm={makeConfirm()}
        onDecide={onDecide}
        onExpire={vi.fn()}
      />,
    );

    await userEvent.click(screen.getByTestId('chat-confirm-approve'));

    expect(onDecide).toHaveBeenCalledTimes(1);
    expect(onDecide).toHaveBeenCalledWith('approve');
  });

  it('calls onDecide("deny") when Deny is clicked', async () => {
    const onDecide = vi.fn().mockResolvedValue(undefined);
    render(
      <ConfirmationCard
        confirm={makeConfirm()}
        onDecide={onDecide}
        onExpire={vi.fn()}
      />,
    );

    await userEvent.click(screen.getByTestId('chat-confirm-deny'));

    expect(onDecide).toHaveBeenCalledTimes(1);
    expect(onDecide).toHaveBeenCalledWith('deny');
  });

  it('disables the buttons while the decision is in flight', async () => {
    let release!: () => void;
    const onDecide = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          release = resolve;
        }),
    );
    render(
      <ConfirmationCard
        confirm={makeConfirm()}
        onDecide={onDecide}
        onExpire={vi.fn()}
      />,
    );

    await userEvent.click(screen.getByTestId('chat-confirm-approve'));

    await waitFor(() => {
      expect(screen.getByTestId('chat-confirm-approve')).toBeDisabled();
    });
    expect(screen.getByTestId('chat-confirm-deny')).toBeDisabled();

    release();
    await waitFor(() => {
      expect(screen.getByTestId('chat-confirm-approve')).not.toBeDisabled();
    });
  });

  it('renders the approved state without buttons', () => {
    render(
      <ConfirmationCard
        confirm={makeConfirm({ decision: 'approved' })}
        onDecide={vi.fn()}
        onExpire={vi.fn()}
      />,
    );

    expect(screen.getByTestId('chat-confirm-card')).toHaveAttribute(
      'data-decision',
      'approved',
    );
    expect(screen.getByTestId('chat-confirm-status')).toHaveTextContent(
      /approved/i,
    );
    expect(screen.queryByTestId('chat-confirm-approve')).toBeNull();
    expect(screen.queryByTestId('chat-confirm-deny')).toBeNull();
  });

  it('renders the denied state without buttons', () => {
    render(
      <ConfirmationCard
        confirm={makeConfirm({ decision: 'denied' })}
        onDecide={vi.fn()}
        onExpire={vi.fn()}
      />,
    );

    expect(screen.getByTestId('chat-confirm-card')).toHaveAttribute(
      'data-decision',
      'denied',
    );
    expect(screen.getByTestId('chat-confirm-status')).toHaveTextContent(
      /denied/i,
    );
    expect(screen.queryByTestId('chat-confirm-approve')).toBeNull();
  });

  it('renders the expired state without buttons', () => {
    // Design.md D2: a resumed run replays its checkpointed
    // confirm_request, whose approval id is dead — the decision POST
    // answers 404 and the store resolves the card as expired.
    render(
      <ConfirmationCard
        confirm={makeConfirm({ decision: 'expired' })}
        onDecide={vi.fn()}
        onExpire={vi.fn()}
      />,
    );

    expect(screen.getByTestId('chat-confirm-card')).toHaveAttribute(
      'data-decision',
      'expired',
    );
    expect(screen.getByTestId('chat-confirm-status')).toHaveTextContent(
      /expired/i,
    );
    expect(screen.queryByTestId('chat-confirm-approve')).toBeNull();
    expect(screen.queryByTestId('chat-confirm-deny')).toBeNull();
  });

  it('keeps the buttons live and shows the hint after a failed POST', () => {
    render(
      <ConfirmationCard
        confirm={makeConfirm({
          error: 'Could not send your decision. Please try again.',
        })}
        onDecide={vi.fn()}
        onExpire={vi.fn()}
      />,
    );

    // Still pending: the user can retry.
    expect(screen.getByTestId('chat-confirm-card')).toHaveAttribute(
      'data-decision',
      'pending',
    );
    expect(screen.getByTestId('chat-confirm-error')).toHaveTextContent(
      /could not send/i,
    );
    expect(screen.getByTestId('chat-confirm-approve')).toBeInTheDocument();
    expect(screen.getByTestId('chat-confirm-deny')).toBeInTheDocument();
  });

  it('renders an empty-object preview when the frame carried no args', () => {
    render(
      <ConfirmationCard
        confirm={makeConfirm({ args: undefined })}
        onDecide={vi.fn()}
        onExpire={vi.fn()}
      />,
    );

    expect(screen.getByTestId('chat-confirm-args')).toHaveTextContent('{}');
  });

  describe('expiry countdown', () => {
    const T0 = new Date('2026-10-15T00:00:00Z').getTime();
    const expiresIn = (seconds: number) => T0 / 1000 + seconds;

    function withFakeClock(run: () => void) {
      vi.useFakeTimers();
      vi.setSystemTime(new Date(T0));
      try {
        run();
      } finally {
        vi.useRealTimers();
      }
    }

    it('shows the remaining time for a fresh card', () => {
      withFakeClock(() => {
        render(
          <ConfirmationCard
            confirm={makeConfirm({ expiresAt: expiresIn(300) })}
            onDecide={vi.fn()}
            onExpire={vi.fn()}
          />,
        );

        expect(screen.getByTestId('chat-confirm-countdown')).toHaveTextContent(
          'Expires in 5:00',
        );
      });
    });

    it('ticks the remaining time down', () => {
      withFakeClock(() => {
        render(
          <ConfirmationCard
            confirm={makeConfirm({ expiresAt: expiresIn(300) })}
            onDecide={vi.fn()}
            onExpire={vi.fn()}
          />,
        );

        act(() => {
          vi.advanceTimersByTime(1_000);
        });

        expect(screen.getByTestId('chat-confirm-countdown')).toHaveTextContent(
          'Expires in 4:59',
        );
      });
    });

    it('invokes onExpire exactly once when the deadline passes', () => {
      withFakeClock(() => {
        const onExpire = vi.fn();
        const confirm = makeConfirm({ expiresAt: expiresIn(2) });
        const { rerender } = render(
          <ConfirmationCard
            confirm={confirm}
            onDecide={vi.fn()}
            onExpire={onExpire}
          />,
        );

        act(() => {
          vi.advanceTimersByTime(5_000);
        });

        expect(onExpire).toHaveBeenCalledTimes(1);
        expect(screen.getByTestId('chat-confirm-countdown')).toHaveTextContent(
          'Expires in 0:00',
        );

        const laterOnExpire = vi.fn();
        rerender(
          <ConfirmationCard
            confirm={confirm}
            onDecide={vi.fn()}
            onExpire={laterOnExpire}
          />,
        );
        act(() => {
          vi.advanceTimersByTime(3_000);
        });

        expect(onExpire).toHaveBeenCalledTimes(1);
        expect(laterOnExpire).not.toHaveBeenCalled();
      });
    });

    it('never invokes onExpire for a decided card', () => {
      withFakeClock(() => {
        const onExpire = vi.fn();
        render(
          <ConfirmationCard
            confirm={makeConfirm({ decision: 'approved', expiresAt: expiresIn(-10) })}
            onDecide={vi.fn()}
            onExpire={onExpire}
          />,
        );

        act(() => {
          vi.advanceTimersByTime(5_000);
        });

        expect(onExpire).not.toHaveBeenCalled();
        expect(screen.queryByTestId('chat-confirm-countdown')).toBeNull();
      });
    });
  });
});
