export default {
  menus: [ // 菜单相关路由
    // { key: '/app/dashboard/index', title: '首页', icon: 'mobile', component: 'Dashboard' },
    {
      key: '/app/cronJobTable/index',
      title: 'Cron Job',
      icon: 'scan',
      component: 'CronJobTable'
    },
    {
      key: '/app/workerTable/index',
      title: 'WorkerPool',
      icon: 'scan',
      component: 'WorkerTable'
    }

  ],
  others: [] // 非菜单相关路由
}
