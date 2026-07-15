export const formatDate = (iso: string): string => {
  const d = new Date(iso);
  return d.toLocaleString('ja-JP', { timeZone: 'Asia/Tokyo' });
};

export const elapsedTime = (startIso: string, endIso?: string): string => {
  const start = new Date(startIso).getTime();
  const end = endIso ? new Date(endIso).getTime() : Date.now();
  const sec = Math.floor((end - start) / 1000);
  if (sec < 60) return `${sec}秒`;
  if (sec < 3600) return `${Math.floor(sec / 60)}分`;
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  return m > 0 ? `${h}時間${m}分` : `${h}時間`;
};
