FROM nginxinc/nginx-unprivileged:alpine
COPY dist/ /usr/share/nginx/html/
# メインの nginx.conf ごと置き換えて /tmp への書き込みを許可する
COPY nginx.conf /etc/nginx/nginx.conf
EXPOSE 9001
