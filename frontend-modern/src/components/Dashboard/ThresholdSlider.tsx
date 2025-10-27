import { createSignal, createEffect, onMount } from 'solid-js';

interface ThresholdSliderProps {
  value: number;
  onChange: (value: number) => void;
  type: 'cpu' | 'memory' | 'disk';
  min?: number;
  max?: number;
}

export function ThresholdSlider(props: ThresholdSliderProps) {
  let sliderRef: HTMLInputElement | undefined;
  let thumbRef: HTMLDivElement | undefined;
  const [thumbPosition, setThumbPosition] = createSignal(0);
  const [isDragging, setIsDragging] = createSignal(false);

  // Color mapping
  const colorMap = {
    cpu: 'text-blue-500',
    memory: 'text-green-500',
    disk: 'text-amber-500',
  };

  // Calculate visual position - allow full range 0-100%
  const calculateVisualPosition = (value: number) => {
    const min = props.min || 0;
    const max = props.max || 100;
    const percent = ((value - min) / (max - min)) * 100;
    // Use full range, handle edge cases with CSS
    return Math.max(0, Math.min(100, percent));
  };

  // Update thumb position when value changes
  createEffect(() => {
    if (sliderRef) {
      setThumbPosition(calculateVisualPosition(props.value));
    }
  });

  onMount(() => {
    // Initialize thumb position
    setThumbPosition(calculateVisualPosition(props.value));
  });

  // Prevent scrolling while dragging
  const handleMouseDown = () => {
    setIsDragging(true);

    // Store the current scroll position
    const scrollY = window.scrollY;
    const scrollX = window.scrollX;

    const handleScroll = () => {
      window.scrollTo(scrollX, scrollY);
    };

    const handleMouseUp = () => {
      setIsDragging(false);
      window.removeEventListener('scroll', handleScroll, { capture: true });
      document.removeEventListener('mouseup', handleMouseUp);
    };

    // Lock scroll position while dragging
    window.addEventListener('scroll', handleScroll, { capture: true });
    document.addEventListener('mouseup', handleMouseUp);
  };

  return (
    <div
      class="relative w-full h-3.5 overflow-visible"
      onWheel={(e) => isDragging() && e.preventDefault()}
      style={{ 'touch-action': isDragging() ? 'none' : 'auto' }}
    >
      {/* Track background */}
      <div class="absolute inset-0 h-3.5 rounded bg-gray-200 dark:bg-gray-600"></div>

      {/* Colored fill */}
      <div
        class={`absolute left-0 h-3.5 rounded ${
          props.type === 'cpu'
            ? 'bg-blue-500/30'
            : props.type === 'memory'
              ? 'bg-green-500/30'
              : 'bg-amber-500/30'
        }`}
        style={{ width: `${calculateVisualPosition(props.value)}%` }}
      ></div>

      {/* Native range input (invisible but functional) */}
      <input
        ref={sliderRef}
        type="range"
        min={props.min || 0}
        max={props.max || 100}
        value={props.value}
        onInput={(e) => props.onChange(parseInt(e.currentTarget.value))}
        onMouseDown={handleMouseDown}
        onWheel={(e) => e.preventDefault()}
        class="absolute inset-0 w-full h-3.5 opacity-0 cursor-pointer z-20"
        style={{ 'touch-action': 'none' }}
        title={`${props.type.toUpperCase()}: ${props.value}%`}
      />

      {/* Custom thumb with value */}
      <div
        ref={thumbRef}
        class={`absolute top-1/2 pointer-events-none z-10 ${colorMap[props.type]}`}
        style={{
          left: `${thumbPosition()}%`,
          transform: `translateY(-50%) translateX(${
            thumbPosition() <= 1
              ? '0%' // At 0-1%, keep at left edge
              : thumbPosition() >= 99
                ? '-100%' // At 99-100%, keep at right edge
                : '-50%' // Otherwise center
          })`,
        }}
      >
        <div class="relative">
          <div class="w-9 h-4 bg-white dark:bg-gray-800 rounded-full shadow-md border-2 border-current flex items-center justify-center">
            <span class="text-[9px] font-semibold">{props.value}%</span>
          </div>
        </div>
      </div>
    </div>
  );
}
